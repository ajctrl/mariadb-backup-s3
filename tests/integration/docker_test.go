//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const defaultMariaDBTestImage = "mariadb:11.4.14@sha256:3142de550b6cd60c30636e2b260eb2f923fe10a5675e88dc86a8ea09eacda8c5"
const defaultS3TestImage = "docker.io/jcometki/minio:RELEASE.2025-09-07T16-13-09Z-multiarch@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e"

// TestDockerBackupRestore requires a local Docker daemon and an already-built
// backup image. All credentials, containers and stored data belong to this test.
func TestDockerBackupRestore(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("integration tests require the docker executable")
	}
	docker(t, ctx, "info", "--format", "{{.ServerVersion}}")
	image := envOr("BACKUP_TEST_IMAGE", "mariadb-backup-s3:integration")
	for flag, want := range map[string]string{"-u": "10001", "-g": "10001"} {
		if got := strings.TrimSpace(docker(t, ctx, "run", "--rm", "--pull=never", image, "id", flag)); got != want {
			t.Fatalf("backup image id %s = %s, want %s", flag, got, want)
		}
	}

	name := fmt.Sprintf("mariadbs3-integration-%d", time.Now().UnixNano())
	docker(t, ctx, "network", "create", name)
	t.Cleanup(func() { dockerCleanup(t, "network", "rm", name) })
	mariadb := name + "-mariadb"
	minio := name + "-s3"
	for _, container := range []string{mariadb, minio} {
		t.Cleanup(func() { dockerCleanup(t, "rm", "-f", "-v", container) })
	}
	docker(t, ctx, "run", "-d", "--name", mariadb, "--network", name, "--network-alias", "mariadb",
		"-p", "127.0.0.1::9000",
		"-e", "MARIADB_ROOT_PASSWORD=integration-password", "-e", "MARIADB_DATABASE=app",
		envOr("MARIADB_TEST_IMAGE", defaultMariaDBTestImage))
	// Sharing one isolated network namespace also works on Docker hosts that
	// disable communication between containers on a bridge network.
	docker(t, ctx, "run", "-d", "--name", minio, "--network", "container:"+mariadb,
		"-e", "MINIO_ROOT_USER=integration", "-e", "MINIO_ROOT_PASSWORD=integration-password",
		envOr("S3_TEST_IMAGE", defaultS3TestImage), "server", "/data")

	waitFor(t, ctx, "MariaDB", func(ctx context.Context) error {
		_, err := exec.CommandContext(ctx, "docker", "exec", mariadb, "mariadb", "--host=127.0.0.1", "--user=root", "--password=integration-password", "--execute=SELECT 1").CombinedOutput()
		return err
	})
	endpoint := "http://" + strings.TrimSpace(docker(t, ctx, "port", mariadb, "9000/tcp"))
	client := s3.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("integration", "integration-password", ""),
		HTTPClient:  &http.Client{Timeout: 5 * time.Second},
	}, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
		o.RetryMaxAttempts = 1
	})
	waitFor(t, ctx, "MinIO", func(ctx context.Context) error {
		_, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
		return err
	})
	bucket := name
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}

	sql := func(t *testing.T, query string) string {
		t.Helper()
		return strings.TrimSpace(docker(t, ctx, "exec", mariadb, "mariadb", "--host=127.0.0.1", "--user=root", "--password=integration-password", "--batch", "--skip-column-names", "--execute", query))
	}
	sql(t, `
		CREATE TABLE app.history_probe (v INT) WITH SYSTEM VERSIONING;
		INSERT INTO app.history_probe VALUES (1);
		UPDATE app.history_probe SET v=2;
		CREATE TABLE app.backup_probe (id INT PRIMARY KEY, value TEXT NOT NULL, payload VARBINARY(32) NOT NULL) ENGINE=InnoDB;
		INSERT INTO app.backup_probe VALUES (1, 'original', UNHEX('0001FF275C0A'));
		CREATE VIEW app.probe_view AS SELECT id, value FROM app.backup_probe;
		CREATE PROCEDURE app.probe_value() READS SQL DATA SELECT value FROM app.backup_probe WHERE id = 1;
		CREATE EVENT app.probe_event ON SCHEDULE EVERY 1 DAY DISABLE DO UPDATE app.backup_probe SET value = 'event';
		CREATE TRIGGER app.probe_insert BEFORE INSERT ON app.backup_probe FOR EACH ROW SET NEW.value = CONCAT(NEW.value, ':trigger');
		CREATE DATABASE analytics;
		CREATE TABLE analytics.backup_probe (id INT PRIMARY KEY, value TEXT NOT NULL) ENGINE=InnoDB;
		INSERT INTO analytics.backup_probe VALUES (1, 'analytics original');
		CREATE DATABASE excluded;
		CREATE TABLE excluded.backup_probe (id INT PRIMARY KEY, value TEXT NOT NULL) ENGINE=InnoDB;
		INSERT INTO excluded.backup_probe VALUES (1, 'excluded original');
	`)
	assertValue := func(t *testing.T, want string) {
		t.Helper()
		if got := sql(t, "SELECT value FROM app.backup_probe WHERE id = 1"); got != want {
			t.Fatalf("restored value = %q, want %q", got, want)
		}
	}
	assertSchema := func(t *testing.T) {
		t.Helper()
		for query, want := range map[string]string{
			"SELECT GROUP_CONCAT(v ORDER BY v) FROM app.history_probe FOR SYSTEM_TIME ALL":                           "1,2",
			"SELECT HEX(payload) FROM app.backup_probe WHERE id = 1":                                                 "0001FF275C0A",
			"SELECT value FROM app.probe_view WHERE id = 1":                                                          "original",
			"CALL app.probe_value()":                                                                                 "original",
			"SELECT STATUS FROM information_schema.EVENTS WHERE EVENT_SCHEMA = 'app' AND EVENT_NAME = 'probe_event'": "DISABLED",
		} {
			if got := sql(t, query); got != want {
				t.Fatalf("restored schema query %q = %q, want %q", query, got, want)
			}
		}
		sql(t, "INSERT INTO app.backup_probe VALUES (2, 'inserted', X'FF')")
		if got := sql(t, "SELECT value FROM app.backup_probe WHERE id = 2"); got != "inserted:trigger" {
			t.Fatalf("restored trigger produced %q", got)
		}
		sql(t, "DELETE FROM app.backup_probe WHERE id = 2")
	}
	baseEnv := map[string]string{
		"MARIADB_HOST": "127.0.0.1", "MARIADB_USER": "root", "MARIADB_PASSWORD": "integration-password",
		"MARIADB_DATABASE": "app", "S3_ALLOW_HTTP": "true", "S3_BUCKET": bucket, "S3_REGION": "us-east-1", "S3_ENDPOINT": "http://127.0.0.1:9000",
		"S3_ACCESS_KEY_ID": "integration", "S3_SECRET_ACCESS_KEY": "integration-password",
	}
	runNumber := 0
	runWithImage := func(t *testing.T, image string, overrides map[string]string, command ...string) (string, error) {
		t.Helper()
		runNumber++
		container := fmt.Sprintf("%s-run-%d", name, runNumber)
		defer dockerCleanup(t, "rm", "-f", "-v", container)
		args := []string{"run", "--rm", "--name", container, "--network", "container:" + mariadb}
		env := make(map[string]string, len(baseEnv)+len(overrides))
		for key, value := range baseEnv {
			env[key] = value
		}
		for key, value := range overrides {
			env[key] = value
		}
		keys := make([]string, 0, len(env))
		for key := range env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			args = append(args, "-e", key+"="+env[key])
		}
		args = append(args, image)
		args = append(args, command...)
		out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	runImage := func(t *testing.T, overrides map[string]string, command ...string) (string, error) {
		t.Helper()
		return runWithImage(t, image, overrides, command...)
	}
	runBackupImage := func(t *testing.T, overrides map[string]string, command ...string) {
		t.Helper()
		out, err := runImage(t, overrides, command...)
		if err != nil {
			t.Fatalf("backup container failed: %v\n%s", err, out)
		}
		t.Log(out)
	}
	listKeys := func(t *testing.T, prefix string) []string {
		t.Helper()
		objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
		if err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(objects.Contents))
		for _, object := range objects.Contents {
			keys = append(keys, aws.ToString(object.Key))
		}
		sort.Strings(keys)
		return keys
	}
	readDump := func(t *testing.T, key string) string {
		t.Helper()
		object, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			t.Fatal(err)
		}
		defer object.Body.Close()
		data, err := io.ReadAll(object.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if previous := os.Getenv("PREVIOUS_BACKUP_IMAGE"); previous != "" {
		t.Run("restore_previous_backup", func(t *testing.T) {
			instance := name + "-previous"
			t.Cleanup(func() { dockerCleanup(t, "rm", "-f", "-v", instance) })
			docker(t, ctx, "run", "-d", "--name", instance, "--network", "container:"+mariadb,
				"-e", "MARIADB_ROOT_PASSWORD=integration-password", "-e", "MARIADB_DATABASE=compat",
				os.Getenv("PREVIOUS_MARIADB_IMAGE"), "--port=3308")
			source := []string{"exec", instance, "mariadb", "--host=127.0.0.1", "--port=3308", "--user=root",
				"--password=integration-password", "--batch", "--skip-column-names", "--execute"}
			waitFor(t, ctx, "previous MariaDB", func(ctx context.Context) error {
				return exec.CommandContext(ctx, "docker", append(source, "SELECT 1")...).Run()
			})
			docker(t, ctx, append(source, `CREATE TABLE compat.records (id INT PRIMARY KEY, payload VARBINARY(16));
				INSERT INTO compat.records VALUES (1, X'00FF'), (2, X'ABCDEF');
				CREATE PROCEDURE compat.read_record(IN n INT) SELECT HEX(payload) FROM compat.records WHERE id=n;`)...)
			const rows = "SELECT id, HEX(payload) FROM compat.records ORDER BY id"
			const routine = "CALL compat.read_record(2)"
			wantRows := strings.TrimSpace(docker(t, ctx, append(source, rows)...))
			wantRoutine := strings.TrimSpace(docker(t, ctx, append(source, routine)...))

			env := map[string]string{"S3_PREFIX": "previous", "MARIADB_DATABASE": "compat", "MARIADB_PORT": "3308"}
			if out, err := runWithImage(t, previous, env, "mariadb-backup-s3", "backup"); err != nil {
				t.Fatalf("previous image backup failed: %v\n%s", err, out)
			}
			delete(env, "MARIADB_PORT") // Restore into the separate, current server.
			runBackupImage(t, env, "mariadb-backup-s3", "restore")
			if got := sql(t, rows); got != wantRows {
				t.Fatalf("restored rows = %q, previous server had %q", got, wantRows)
			}
			if got := sql(t, routine); got != wantRoutine {
				t.Fatalf("restored procedure returned %q, previous server returned %q", got, wantRoutine)
			}
			sql(t, "DROP DATABASE compat")
		})
	}
	if !t.Run("timestamp_plaintext", func(t *testing.T) {
		env := map[string]string{"S3_PREFIX": "timestamp"}
		// Exercise the default CMD as well as the restore wrapper.
		runBackupImage(t, env)
		keys := listKeys(t, "timestamp/")
		if len(keys) != 1 || !strings.HasPrefix(keys[0], "timestamp/app/") || !strings.HasSuffix(keys[0], ".sql") {
			t.Fatalf("expected one timestamped SQL dump, got %v", keys)
		}
		if dump := readDump(t, keys[0]); !strings.Contains(dump, "CREATE TABLE `backup_probe`") || !strings.Contains(dump, "0x0001FF275C0A") {
			t.Fatal("backup did not contain table SQL and binary data")
		}
		sourceCollation := sql(t, "SELECT DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='app'")
		sql(t, "DROP DATABASE app; CREATE DATABASE app CHARACTER SET latin1 COLLATE latin1_bin; CREATE TABLE app.keep_me(id INT); INSERT INTO app.keep_me VALUES(42)")
		out, err := runImage(t, env, "sh", "restore.sh")
		if err == nil || !strings.Contains(out, sourceCollation) || !strings.Contains(out, "latin1_bin") || sql(t, "SELECT id FROM app.keep_me") != "42" {
			t.Fatalf("restore did not stop before changing the mismatched destination: %v\n%s", err, out)
		}
		sql(t, "DROP DATABASE app")
		runBackupImage(t, env, "sh", "restore.sh")
		if got := sql(t, "SELECT DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='app'"); got != sourceCollation {
			t.Fatalf("created database collation = %q, want %q", got, sourceCollation)
		}
		assertValue(t, "original")
		assertSchema(t)
	}) {
		return
	}
	if !t.Run("fixed_fallback_restore", func(t *testing.T) {
		env := map[string]string{"S3_PREFIX": "fallback", "BACKUP_FILENAME_MODE": "fixed"}
		runBackupImage(t, env)
		keys := listKeys(t, "fallback/")
		if len(keys) != 1 || !strings.HasPrefix(keys[0], "fallback/app/") || !strings.HasSuffix(keys[0], ".sql") {
			t.Fatalf("expected one fallback dump, got %v", keys)
		}
		timestamp := strings.TrimSuffix(strings.TrimPrefix(keys[0], "fallback/app/"), ".sql")
		sql(t, "UPDATE app.backup_probe SET value = 'changed'")
		env["BACKUP_FILENAME_MODE"] = "timestamp"
		runBackupImage(t, env, "mariadb-backup-s3", "restore", timestamp)
		assertValue(t, "original")
	}) {
		return
	}
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		t.Fatal(err)
	}
	if !t.Run("per_database_selected", func(t *testing.T) {
		env := map[string]string{
			"S3_PREFIX": "selected", "BACKUP_MODE": "per-database", "BACKUP_FILENAME_MODE": "fixed",
			"MARIADB_DATABASE": "", "MARIADB_DATABASES": "app,analytics",
		}
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		if keys := listKeys(t, "selected/"); strings.Join(keys, ",") != "selected/analytics_latest.sql,selected/app_latest.sql" {
			t.Fatalf("unexpected separate database objects: %v", keys)
		}
		sql(t, "UPDATE app.backup_probe SET value = 'changed'; UPDATE analytics.backup_probe SET value = 'changed'")
		env["MARIADB_DATABASES"] = ""
		env["MARIADB_DATABASE"] = "app"
		runBackupImage(t, env, "mariadb-backup-s3", "restore")
		assertValue(t, "original")
		if got := sql(t, "SELECT value FROM analytics.backup_probe WHERE id = 1"); got != "changed" {
			t.Fatalf("restoring app also changed analytics: %q", got)
		}
		env["MARIADB_DATABASE"] = "analytics"
		runBackupImage(t, env, "mariadb-backup-s3", "restore")
		if got := sql(t, "SELECT value FROM analytics.backup_probe WHERE id = 1"); got != "analytics original" {
			t.Fatalf("analytics restore = %q", got)
		}
	}) {
		return
	}
	for _, allDatabases := range []bool{false, true} {
		testName := "combined_selected"
		if allDatabases {
			testName = "combined_all_excluded"
		}
		if !t.Run(testName, func(t *testing.T) {
			env := map[string]string{"S3_PREFIX": testName, "BACKUP_MODE": "combined", "MARIADB_DATABASE": ""}
			if allDatabases {
				env["MARIADB_BACKUP_ALL"] = "true"
				env["MARIADB_DATABASES_EXCLUDE"] = "excluded"
			} else {
				env["MARIADB_DATABASES"] = "app,analytics"
				env["BACKUP_FILENAME_MODE"] = "fixed"
			}
			runBackupImage(t, env, "mariadb-backup-s3", "backup")
			keys := listKeys(t, testName+"/")
			if len(keys) != 1 || !strings.HasPrefix(keys[0], testName+"/") || !strings.HasSuffix(keys[0], ".sql") {
				t.Fatalf("expected one combined SQL dump, got %v", keys)
			}
			dump := readDump(t, keys[0])
			for _, database := range []string{"app", "analytics"} {
				if !strings.Contains(dump, "USE `"+database+"`;") {
					t.Fatalf("combined dump omitted database %s", database)
				}
			}
			for _, database := range []string{"excluded", "mysql", "sys", "information_schema", "performance_schema"} {
				if strings.Contains(dump, "USE `"+database+"`;") {
					t.Fatalf("combined dump included excluded database %s", database)
				}
			}
			sql(t, "DROP DATABASE app; DROP DATABASE analytics")
			env["MARIADB_DATABASES"] = ""
			env["MARIADB_BACKUP_ALL"] = "false"
			env["MARIADB_DATABASES_EXCLUDE"] = ""
			runBackupImage(t, env, "mariadb-backup-s3", "restore")
			assertValue(t, "original")
			assertSchema(t)
			if got := sql(t, "SELECT value FROM analytics.backup_probe WHERE id = 1"); got != "analytics original" {
				t.Fatalf("combined restore omitted analytics data: %q", got)
			}
			if got := sql(t, "SELECT value FROM excluded.backup_probe WHERE id = 1"); got != "excluded original" {
				t.Fatalf("combined restore affected excluded database: %q", got)
			}
		}) {
			return
		}
	}
	t.Run("quoted_credentials", func(t *testing.T) {
		sql(t, `CREATE USER 'optionfile'@'%' IDENTIFIED BY '"quote" \'single\' \\ slash\n#comment\tend'; GRANT ALL PRIVILEGES ON *.* TO 'optionfile'@'%';`)
		env := map[string]string{
			"S3_PREFIX": "optionfile", "BACKUP_FILENAME_MODE": "fixed", "MARIADB_DATABASE": "analytics",
			"MARIADB_USER": "optionfile", "MARIADB_PASSWORD": "\"quote\" 'single' \\ slash\n#comment\tend",
		}
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		sql(t, "UPDATE analytics.backup_probe SET value = 'changed after quoted-credential backup'")
		runBackupImage(t, env, "mariadb-backup-s3", "restore")
		if got := sql(t, "SELECT value FROM analytics.backup_probe WHERE id = 1"); got != "analytics original" {
			t.Fatalf("quoted-credential restore = %q", got)
		}
	})
	t.Run("fixed_encrypted_versions", func(t *testing.T) {
		env := map[string]string{"S3_PREFIX": "fixed/", "BACKUP_FILENAME_MODE": "fixed", "PASSPHRASE": " integration \"quote\" 'single' \\ tab\treturn\r end "}
		key := "fixed/app_latest.sql.gpg"
		version := func() string {
			t.Helper()
			object, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
			if err != nil {
				t.Fatal(err)
			}
			if aws.ToString(object.VersionId) == "" || aws.ToString(object.VersionId) == "null" || aws.ToInt64(object.ContentLength) == 0 {
				t.Fatalf("expected a nonempty versioned encrypted object, got %+v", object)
			}
			return *object.VersionId
		}
		sql(t, "UPDATE app.backup_probe SET value = 'first encrypted version'")
		// Run twice in one container: the GPG agent survives the first command,
		// and must not keep its inherited file descriptor locked for the next run.
		runBackupImage(t, env, "sh", "-c", "sh backup.sh && sh backup.sh")
		firstVersion := version()
		sql(t, "UPDATE app.backup_probe SET value = 'second encrypted version'")
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		secondVersion := version()
		if firstVersion == secondVersion {
			t.Fatal("second backup did not create a new S3 version")
		}
		sql(t, "UPDATE app.backup_probe SET value = 'changed after both backups'")
		runBackupImage(t, env, "sh", "restore.sh", "--version-id", firstVersion)
		assertValue(t, "first encrypted version")
		runBackupImage(t, env, "mariadb-backup-s3", "restore")
		assertValue(t, "second encrypted version")
	})
	t.Run("streaming_encrypted_multipart", func(t *testing.T) {
		// Incompressible enough to cross the multipart threshold even after GPG.
		sql(t, "CREATE TABLE app.stream_probe ENGINE=InnoDB AS SELECT seq AS id, CONCAT(MD5(seq), MD5(seq + 1000000)) AS value FROM app.seq_1_to_400000")
		checksumQuery := "SELECT CONCAT(COUNT(*), ':', SUM(CRC32(CONCAT(id, ':', value)))) FROM app.stream_probe"
		want := sql(t, checksumQuery)
		env := map[string]string{
			"S3_PREFIX": "streaming", "BACKUP_FILENAME_MODE": "fixed", "PASSPHRASE": "streaming passphrase",
			"S3_UPLOAD_PART_SIZE_MB": "5", "TMPDIR": "/proc",
		}
		// /proc cannot hold temporary dump files. Only streaming can succeed here.
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		object, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("streaming/app_latest.sql.gpg")})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToInt64(object.ContentLength) <= 5*1024*1024 || !strings.Contains(aws.ToString(object.ETag), "-") {
			t.Fatalf("expected an encrypted multipart object: size=%d etag=%s", aws.ToInt64(object.ContentLength), aws.ToString(object.ETag))
		}
		sql(t, "TRUNCATE app.stream_probe")
		// Restore still validates downloaded/decrypted files before touching the DB.
		env["TMPDIR"] = "/tmp"
		runBackupImage(t, env, "mariadb-backup-s3", "restore")
		if got := sql(t, checksumQuery); got != want {
			t.Fatalf("streaming round trip changed data: got %q, want %q", got, want)
		}
	})
	t.Run("restricted_privileges_and_case_sensitive_names", func(t *testing.T) {
		sql(t, `CREATE DATABASE Sys;
			CREATE TABLE Sys.Orders(id INT); CREATE TABLE Sys.orders(id INT);
			CREATE TRIGGER Sys.record_insert BEFORE INSERT ON Sys.orders FOR EACH ROW SET NEW.id=10;
			CREATE USER 'backup_test'@'%' IDENTIFIED BY 'limited';
			GRANT SELECT, SHOW VIEW, EVENT ON Sys.* TO 'backup_test'@'%';
			GRANT SELECT ON mysql.proc TO 'backup_test'@'%';`)
		env := map[string]string{"MARIADB_USER": "backup_test", "MARIADB_PASSWORD": "limited",
			"MARIADB_DATABASE": "", "MARIADB_BACKUP_ALL": "true", "S3_PREFIX": "case-test"}
		checkFailure := func(want string) {
			t.Helper()
			out, err := runImage(t, env, "mariadb-backup-s3", "backup")
			if err == nil || !strings.Contains(out, want) || len(listKeys(t, "case-test/Sys/")) != 0 {
				t.Fatalf("expected %q before publication; err=%v output=%s", want, err, out)
			}
		}
		checkFailure("TRIGGER")
		sql(t, `CREATE ROLE backup_trigger;
			CREATE ROLE backup_reader;
			GRANT TRIGGER ON Sys.orders TO backup_trigger;
			GRANT backup_trigger TO backup_reader;
			GRANT backup_reader TO 'backup_test'@'%';`)
		checkFailure("TRIGGER")
		sql(t, "SET DEFAULT ROLE backup_reader FOR 'backup_test'@'%'")
		checkFailure("Sys.Orders")
		// Across the role hierarchy, a specific schema grant shadows a broader
		// wildcard. Both grant rows must participate in the preflight.
		sql(t, "GRANT SELECT ON Sys.* TO backup_reader; GRANT TRIGGER ON `S%`.* TO backup_trigger")
		checkFailure("Sys.Orders")
		sql(t, "GRANT TRIGGER ON Sys.Orders TO PUBLIC")
		for _, opt := range []string{"--ignore-table=", "--ignore-table-data="} {
			env["MARIADB_DUMP_EXTRA_OPTS"] = opt + "Sys.orders"
			checkFailure(`dump exclusion also matches "Sys.Orders"`)
		}
		delete(env, "MARIADB_DUMP_EXTRA_OPTS")
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		keys := listKeys(t, "case-test/Sys/")
		if len(keys) != 1 {
			t.Fatal("discovery omitted the accessible Sys database")
		}
		if !strings.Contains(readDump(t, keys[0]), "record_insert") {
			t.Fatal("published dump is missing trigger record_insert")
		}
	})
	t.Run("case_insensitive_database_selectors", func(t *testing.T) {
		instance := name + "-fold"
		t.Cleanup(func() { dockerCleanup(t, "rm", "-f", "-v", instance) })
		docker(t, ctx, "run", "-d", "--name", instance, "--network", "container:"+mariadb,
			"-e", "MARIADB_ROOT_PASSWORD=integration-password", "-e", "MARIADB_DATABASE=SalesData",
			envOr("MARIADB_TEST_IMAGE", defaultMariaDBTestImage), "--port=3307", "--lower-case-table-names=1")
		waitFor(t, ctx, "case-insensitive MariaDB", func(ctx context.Context) error {
			return exec.CommandContext(ctx, "docker", "exec", instance, "mariadb", "--protocol=tcp", "-h127.0.0.1", "-P3307", "-uroot", "-pintegration-password", "-e", "SELECT 1").Run()
		})
		docker(t, ctx, "exec", instance, "mariadb", "--protocol=tcp", "-h127.0.0.1", "-P3307", "-uroot", "-pintegration-password", "-e", "CREATE DATABASE keepdb")
		env := map[string]string{"MARIADB_PORT": "3307", "MARIADB_DATABASE": "", "MARIADB_BACKUP_ALL": "true", "MARIADB_DATABASES_EXCLUDE": "SalesData", "S3_PREFIX": "fold-discovery", "BACKUP_FILENAME_MODE": "fixed"}
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		keys := listKeys(t, "fold-discovery/")
		if !slices.Contains(keys, "fold-discovery/keepdb_latest.sql") || slices.Contains(keys, "fold-discovery/salesdata_latest.sql") {
			t.Fatalf("mixed-case exclusion: %v", keys)
		}
		env["S3_PREFIX"] = "fold"
		delete(env, "MARIADB_DATABASES_EXCLUDE")
		env["MARIADB_BACKUP_ALL"] = "false"
		env["MARIADB_DATABASES"] = "SalesData,salesdata"
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		if keys := listKeys(t, "fold/"); strings.Join(keys, ",") != "fold/SalesData_latest.sql" {
			t.Fatalf("duplicate selector keys: %v", keys)
		}
	})
	t.Run("visibility_warnings_and_direct_schema_priority", func(t *testing.T) {
		sql(t, "CREATE DATABASE visibility; CREATE TABLE visibility.exposed(id INT); CREATE TABLE visibility.hidden(id INT); CREATE PROCEDURE visibility.hidden_proc() SELECT 1; CREATE USER 'visibility_test'@'%' IDENTIFIED BY 'limited'; GRANT EVENT ON visibility.* TO 'visibility_test'@'%'; GRANT SELECT, TRIGGER ON visibility.exposed TO 'visibility_test'@'%'")
		env := map[string]string{"MARIADB_DATABASE": "visibility", "MARIADB_USER": "visibility_test", "MARIADB_PASSWORD": "limited", "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": "visibility"}
		out, err := runImage(t, env, "mariadb-backup-s3", "backup")
		if err != nil || !strings.Contains(out, "schema-wide SELECT") || !strings.Contains(out, "routine definitions") {
			t.Fatalf("missing visibility warnings: %v %s", err, out)
		}
		key := "visibility/visibility_latest.sql"
		if dump := readDump(t, key); !strings.Contains(dump, "exposed") || strings.Contains(dump, "hidden") {
			t.Fatal("restricted dump differs from account visibility")
		}
		sql(t, "GRANT SELECT, TRIGGER, SHOW VIEW ON visibility.* TO 'visibility_test'@'%'; GRANT SELECT ON `visib%`.* TO 'visibility_test'@'%'")
		// MariaDB 10.11 predates the schema-level SHOW CREATE ROUTINE privilege.
		if strings.HasPrefix(sql(t, "SELECT VERSION()"), "10.11.") {
			sql(t, "GRANT SELECT ON mysql.proc TO 'visibility_test'@'%'")
		} else {
			sql(t, "GRANT SHOW CREATE ROUTINE ON visibility.* TO 'visibility_test'@'%'")
		}
		out, err = runImage(t, env, "mariadb-backup-s3", "backup")
		if err != nil || strings.Contains(out, "cannot verify") {
			t.Fatalf("complete access: %v %s", err, out)
		}
		if dump := readDump(t, key); !strings.Contains(dump, "`hidden`") || !strings.Contains(dump, "hidden_proc") {
			t.Fatal("complete backup lacks table or routine")
		}
		// Reversing the specific and wildcard grants must not grant TRIGGER.
		sql(t, "REVOKE TRIGGER ON visibility.* FROM 'visibility_test'@'%'; GRANT TRIGGER ON `visib%`.* TO 'visibility_test'@'%'")
		if out, err := runImage(t, env, "mariadb-backup-s3", "backup"); err == nil || !strings.Contains(out, "TRIGGER") {
			t.Fatalf("wildcard shadowed by direct grant: %v %s", err, out)
		}
	})
	t.Run("restore_session_owns_lock_and_redacts_errors", func(t *testing.T) {
		idle := sql(t, "SELECT @@GLOBAL.wait_timeout")
		defer sql(t, "SET GLOBAL wait_timeout="+idle)
		sql(t, "SET GLOBAL wait_timeout=1; CREATE TABLE app.import_owner(owns_lock INT)")
		env := map[string]string{"S3_PREFIX": "import-lock", "BACKUP_FILENAME_MODE": "fixed"}
		put := func(body string) {
			_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: &bucket, Key: aws.String("import-lock/app_latest.sql"), Body: strings.NewReader(body)})
			if err != nil {
				t.Fatal(err)
			}
		}
		put("DO SLEEP(2); INSERT INTO app.import_owner SELECT IFNULL(IS_USED_LOCK('mariadb-backup-s3')=CONNECTION_ID(),0);")
		runBackupImage(t, env, "mariadb-backup-s3", "restore")
		if got := sql(t, "SELECT owns_lock FROM app.import_owner"); got != "1" {
			t.Fatalf("import executed without owning the lock: %s", got)
		}
		put("CREATE TEMPORARY TABLE duplicate_key (v VARCHAR(40) PRIMARY KEY);\nINSERT INTO duplicate_key VALUES ('private-record'),('private-record');")
		out, err := runImage(t, env, "mariadb-backup-s3", "restore")
		if err == nil || !strings.Contains(out, "ERROR 1062 (23000) at line 2") || strings.Contains(out, "private-record") {
			t.Fatalf("unexpected restore diagnostic: %v %s", err, out)
		}
		env["RESTORE_LOG_ERROR_DETAILS"] = "true"
		out, err = runImage(t, env, "mariadb-backup-s3", "restore")
		if err == nil || !strings.Contains(out, "private-record") {
			t.Fatalf("explicit detailed diagnostics: %v %s", err, out)
		}
	})
	t.Run("oversized_blob_preserves_destination", func(t *testing.T) {
		packet := sql(t, "SELECT @@GLOBAL.max_allowed_packet")
		defer sql(t, "SET GLOBAL max_allowed_packet="+packet)
		sql(t, "SET GLOBAL max_allowed_packet=1048576; CREATE DATABASE packet_test; CREATE TABLE packet_test.big(v LONGBLOB); INSERT INTO packet_test.big VALUES(REPEAT('x',700000))")
		env := map[string]string{"MARIADB_DATABASE": "packet_test", "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": "packet"}
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		out, err := runImage(t, env, "mariadb-backup-s3", "restore")
		if err == nil || !strings.Contains(out, "max_allowed_packet") {
			t.Fatalf("packet preflight: %v %s", err, out)
		}
		if got := sql(t, "SELECT LENGTH(v) FROM packet_test.big"); got != "700000" {
			t.Fatalf("failed preflight changed existing table: %q", got)
		}
		sql(t, "SET GLOBAL max_allowed_packet=2097152; TRUNCATE packet_test.big")
		runBackupImage(t, env, "mariadb-backup-s3", "restore")
		if got := sql(t, "SELECT LENGTH(v) FROM packet_test.big"); got != "700000" {
			t.Fatalf("restore with sufficient packet size: %q", got)
		}
	})
	t.Run("schema_grant_wildcards_match_bytes", func(t *testing.T) {
		sql(t, `CREATE DATABASE aé1;
			CREATE TABLE aé1.items(id INT);
			CREATE TRIGGER aé1.set_id BEFORE INSERT ON aé1.items FOR EACH ROW SET NEW.id=1;
			CREATE USER 'wildcard_test'@'%' IDENTIFIED BY 'limited';
			GRANT SELECT, SHOW VIEW, EVENT ON *.* TO 'wildcard_test'@'%';
			GRANT TRIGGER ON a_1.* TO 'wildcard_test'@'%';`)
		env := map[string]string{"MARIADB_USER": "wildcard_test", "MARIADB_PASSWORD": "limited",
			"MARIADB_DATABASE": "aé1", "S3_PREFIX": "wildcard-test"}
		out, err := runImage(t, env, "mariadb-backup-s3", "backup")
		if err == nil || !strings.Contains(out, "TRIGGER") || len(listKeys(t, "wildcard-test/")) != 0 {
			t.Fatalf("single underscore covered a two-byte identifier: err=%v output=%s", err, out)
		}
		sql(t, "GRANT TRIGGER ON a__1.* TO 'wildcard_test'@'%'")
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		keys := listKeys(t, "wildcard-test/")
		if len(keys) != 1 || !strings.Contains(readDump(t, keys[0]), "set_id") {
			t.Fatal("backup with matching two-byte grant did not publish the trigger")
		}
	})
}

func docker(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func dockerCleanup(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such container") && !strings.Contains(string(out), "not found") {
		t.Logf("cleanup docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func waitFor(t *testing.T, ctx context.Context, service string, probe func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var lastErr error
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
		lastErr = probe(probeCtx)
		probeCancel()
		if lastErr == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v (last probe: %v)", service, ctx.Err(), lastErr)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
