//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const defaultS3TestImage = "quay.io/minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e"

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
	docker(t, ctx, "image", "inspect", image)

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
		envOr("MARIADB_TEST_IMAGE", "mariadb:11.4"))
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
			"SELECT HEX(payload) FROM app.backup_probe WHERE id = 1": "0001FF275C0A",
			"SELECT value FROM app.probe_view WHERE id = 1":          "original",
			"CALL app.probe_value()":                                 "original",
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
		"MARIADB_DATABASE": "app", "S3_BUCKET": bucket, "S3_REGION": "us-east-1", "S3_ENDPOINT": "http://127.0.0.1:9000",
		"S3_ACCESS_KEY_ID": "integration", "S3_SECRET_ACCESS_KEY": "integration-password",
	}
	runNumber := 0
	runBackupImage := func(t *testing.T, overrides map[string]string, command ...string) {
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
		t.Log(strings.TrimSpace(docker(t, ctx, args...)))
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
		sql(t, "DROP DATABASE app; CREATE DATABASE app")
		runBackupImage(t, env, "sh", "restore.sh")
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
