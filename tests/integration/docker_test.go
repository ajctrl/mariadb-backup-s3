//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ajctrl/mariadb-backup-s3/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Silo RELEASE.2026-10-18T00-00-00Z-distroless, pinned for reproducible tests.
const defaultS3TestImage = "pgsty/silo@sha256:472f59c7be537571f0f9a2ee6b8fe033a220939d521255bd4bf5339e53db7218"

// TestDockerBackupRestore requires a local Docker daemon and an already-built
// backup image. All credentials, containers and stored data belong to this test.
func TestDockerBackupRestore(t *testing.T) {
	testDockerBackupRestore(t, 0)
}

func TestDockerCaseInsensitiveHistory(t *testing.T) {
	testDockerBackupRestore(t, 1)
}

func testDockerBackupRestore(t *testing.T, lowerCaseTableNames int) {
	t.Helper()
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
	s3Container := name + "-s3"
	for _, container := range []string{mariadb, s3Container} {
		t.Cleanup(func() { dockerCleanup(t, "rm", "-f", "-v", container) })
	}
	docker(t, ctx, "run", "-d", "--name", mariadb, "--network", name, "--network-alias", "mariadb",
		"-p", "127.0.0.1::9000",
		"-e", "MARIADB_ROOT_PASSWORD=integration-password", "-e", "MARIADB_DATABASE=app",
		envOr("MARIADB_TEST_IMAGE", "mariadb:11.4"), fmt.Sprintf("--lower-case-table-names=%d", lowerCaseTableNames))
	// Sharing one isolated network namespace also works on Docker hosts that
	// disable communication between containers on a bridge network.
	docker(t, ctx, "run", "-d", "--name", s3Container, "--network", "container:"+mariadb,
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
	waitFor(t, ctx, "S3", func(ctx context.Context) error {
		_, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
		return err
	})
	bucket := name
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
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
		CREATE TABLE app.history_probe (id INT PRIMARY KEY, value VARCHAR(64) NOT NULL) ENGINE=InnoDB WITH SYSTEM VERSIONING;
		INSERT INTO app.history_probe VALUES (1, 'before update'), (2, 'before delete');
		UPDATE app.history_probe SET value = 'current' WHERE id = 1;
		DELETE FROM app.history_probe WHERE id = 2;
		CREATE DATABASE analytics;
		CREATE TABLE analytics.backup_probe (id INT PRIMARY KEY, value TEXT NOT NULL) ENGINE=InnoDB;
		INSERT INTO analytics.backup_probe VALUES (1, 'analytics original');
		CREATE DATABASE excluded;
		CREATE TABLE excluded.backup_probe (id INT PRIMARY KEY, value TEXT NOT NULL) ENGINE=InnoDB;
		INSERT INTO excluded.backup_probe VALUES (1, 'excluded original');
	`)
	const historyQuery = "SELECT id, value, ROW_START, ROW_END FROM app.history_probe FOR SYSTEM_TIME ALL ORDER BY id, ROW_START, ROW_END"
	wantHistory := sql(t, historyQuery)
	if got := sql(t, "SELECT COUNT(*) FROM app.history_probe FOR SYSTEM_TIME ALL"); got != "3" {
		t.Fatalf("history fixture has %s rows, want 3", got)
	}
	assertHistory := func(t *testing.T) {
		t.Helper()
		if got := sql(t, historyQuery); got != wantHistory {
			t.Fatalf("restore changed historical rows or their validity periods:\ngot: %s\nwant: %s", got, wantHistory)
		}
		if got := sql(t, "SELECT id, value FROM app.history_probe ORDER BY id"); got != "1\tcurrent" {
			t.Fatalf("restored current rows = %q", got)
		}
	}
	assertValue := func(t *testing.T, want string) {
		t.Helper()
		assertHistory(t)
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
	runImage := func(t *testing.T, overrides map[string]string, command ...string) (string, error) {
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
		output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
		return strings.TrimSpace(string(output)), err
	}
	runBackupImage := func(t *testing.T, overrides map[string]string, command ...string) {
		t.Helper()
		output, err := runImage(t, overrides, command...)
		if err != nil {
			t.Fatalf("backup image failed: %v\n%s", err, output)
		}
		t.Log(output)
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
	t.Run("generation_collision_preserves_existing_object", func(t *testing.T) {
		store, err := storage.New(ctx, storage.Config{
			Bucket: bucket, Region: "us-east-1", Endpoint: endpoint,
			AccessKeyID: "integration", SecretAccessKey: "integration-password",
			UploadPartSizeBytes: 5 * 1024 * 1024,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, size := range []int{100, 6 * 1024 * 1024} {
			t.Run(fmt.Sprint(size), func(t *testing.T) {
				key := fmt.Sprintf("collision-probe/%d.sql", size)
				original := strings.Repeat("a", size)
				options := storage.UploadOptions{RequireAbsent: true}
				if err := store.Upload(ctx, key, io.NopCloser(strings.NewReader(original)), options); err != nil {
					t.Fatal(err)
				}
				err := store.Upload(ctx, key, io.NopCloser(strings.NewReader(strings.Repeat("b", size))), options)
				var apiErr smithy.APIError
				if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "PreconditionFailed" {
					t.Fatalf("expected conditional write rejection, got %v", err)
				}
				if readDump(t, key) != original {
					t.Fatal("collision replaced the current object")
				}
				versions, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket), Prefix: aws.String(key)})
				if err != nil {
					t.Fatal(err)
				}
				if len(versions.Versions) != 1 {
					t.Fatalf("collision created another version: %d versions", len(versions.Versions))
				}
				uploads, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket), Prefix: aws.String(key)})
				if err != nil {
					t.Fatal(err)
				}
				if len(uploads.Uploads) != 0 {
					t.Fatal("collision left an incomplete multipart upload")
				}
			})
		}
	})
	t.Run("restore_error_does_not_echo_sql", func(t *testing.T) {
		const marker = "RESTORE_ROW_SECRET_MUST_NOT_BE_LOGGED"
		sql(t, "CREATE DATABASE error_log_probe")
		defer sql(t, "DROP DATABASE error_log_probe")
		sql(t, "CREATE USER 'restore_no_insert'@'%' IDENTIFIED BY 'integration-password'")
		defer sql(t, "DROP USER 'restore_no_insert'@'%'")
		// Let the real restore reach INSERT, but make that statement fail.
		sql(t, "GRANT SELECT, CREATE, DROP, ALTER, INDEX, LOCK TABLES ON error_log_probe.* TO 'restore_no_insert'@'%'")
		for _, mode := range []string{"per-database", "combined"} {
			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/encrypted=%t", mode, encrypted), func(t *testing.T) {
					sql(t, "DROP TABLE IF EXISTS error_log_probe.items; CREATE TABLE error_log_probe.items (secret VARCHAR(100)); INSERT INTO error_log_probe.items VALUES ('"+marker+"')")
					env := map[string]string{
						"MARIADB_DATABASE": "error_log_probe", "BACKUP_MODE": mode, "BACKUP_FILENAME_MODE": "fixed",
						"S3_PREFIX": fmt.Sprintf("error-log-probe/%s/%t", mode, encrypted),
					}
					if encrypted {
						env["PASSPHRASE"] = "error log test passphrase"
					}
					runBackupImage(t, env, "mariadb-backup-s3", "backup")
					env["MARIADB_USER"] = "restore_no_insert"
					if mode == "combined" {
						env["MARIADB_DATABASE"] = ""
					}
					output, err := runImage(t, env, "mariadb-backup-s3", "restore")
					if err == nil || !strings.Contains(output, "ERROR 1142") || !strings.Contains(output, "INSERT command denied") || strings.Contains(output, "Restore complete") {
						t.Fatalf("restore did not report the expected INSERT failure: %v\n%s", err, output)
					}
					if strings.Contains(output, marker) || strings.Contains(output, "INSERT INTO") {
						t.Fatal("failed restore echoed SQL or row data to the log")
					}
				})
			}
		}
	})
	t.Run("restore_mixed_charsets", func(t *testing.T) {
		// The dump mixes UTF8 table data with routines in their original
		// encodings. Several characters end in 0x5c (an ASCII backslash).
		sql(t, "CREATE DATABASE charset_probe CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
		defer sql(t, "DROP DATABASE IF EXISTS charset_probe")
		sql(t, "SET NAMES utf8mb4; CREATE TABLE charset_probe.utf8_rows (value TEXT); INSERT INTO charset_probe.utf8_rows VALUES ('日本語と😀')")
		for _, tc := range []struct{ charset, value string }{
			{"sjis", "\x95\x5c"}, {"cp932", "\x95\x5c"}, {"big5", "\xa4\x5c"}, {"gbk", "\x81\x5c"},
			{"latin1", "\xe9"}, {"utf8mb4", "表😀"}, {"ujis", "\xc9\xbd"}, {"euckr", "\xb0\xa1"},
		} {
			sql(t, "CREATE TABLE charset_probe.t_"+tc.charset+" (value TEXT)")
			// A different connection collation catches accidental SET NAMES
			// side effects when synchronizing the restore client's lexer.
			query := "USE charset_probe; SET collation_connection=utf8mb4_bin;\n"
			if tc.charset == "cp932" {
				query += "SET sql_mode='NO_BACKSLASH_ESCAPES';\n"
			}
			query += "CREATE FUNCTION f_" + tc.charset + "() RETURNS TEXT CHARACTER SET utf8mb4 DETERMINISTIC RETURN '" + tc.value + "';\n"
			query += "CREATE PROCEDURE p_" + tc.charset + "() SELECT HEX(CONVERT('" + tc.value + "' USING utf8mb4));\n"
			query += "CREATE TRIGGER tr_" + tc.charset + " BEFORE INSERT ON t_" + tc.charset + " FOR EACH ROW SET NEW.value='" + tc.value + "';\n"
			query += "CREATE EVENT ev_" + tc.charset + " ON SCHEDULE EVERY 1 DAY DISABLE DO INSERT INTO t_" + tc.charset + " VALUES('" + tc.value + "');\n"
			command := exec.CommandContext(ctx, "docker", "exec", "-i", mariadb, "mariadb", "--user=root", "--password=integration-password", "--default-character-set="+tc.charset)
			command.Stdin = strings.NewReader(query)
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("create %s objects: %v\n%s", tc.charset, err, output)
			}
		}
		// U+301C encodes as 0x81 0x60 in SJIS. The trail backtick must
		// remain part of the definer, rather than consuming the closing quote.
		sql(t, "SET NAMES utf8mb4; CREATE USER '〜'@'localhost'")
		defer sql(t, "SET NAMES utf8mb4; DROP USER '〜'@'localhost'")
		command := exec.CommandContext(ctx, "docker", "exec", "-i", mariadb, "mariadb", "--user=root", "--password=integration-password", "--default-character-set=sjis")
		command.Stdin = strings.NewReader("CREATE DEFINER=`\x81\x60`@`localhost` PROCEDURE charset_probe.p_definer() SQL SECURITY INVOKER SELECT 42;")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create SJIS definer: %v\n%s", err, output)
		}
		queries := []string{
			"SELECT HEX(value) FROM charset_probe.utf8_rows",
			"CALL charset_probe.p_definer()",
			"SELECT ROUTINE_NAME, ROUTINE_TYPE, HEX(DEFINER), SQL_MODE, CHARACTER_SET_CLIENT, COLLATION_CONNECTION, DATABASE_COLLATION, HEX(ROUTINE_DEFINITION) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA='charset_probe' ORDER BY ROUTINE_NAME",
			"SELECT TRIGGER_NAME, SQL_MODE, CHARACTER_SET_CLIENT, COLLATION_CONNECTION, DATABASE_COLLATION, HEX(ACTION_STATEMENT) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA='charset_probe' ORDER BY TRIGGER_NAME",
			"SELECT EVENT_NAME, SQL_MODE, CHARACTER_SET_CLIENT, COLLATION_CONNECTION, DATABASE_COLLATION, HEX(EVENT_DEFINITION), STATUS FROM information_schema.EVENTS WHERE EVENT_SCHEMA='charset_probe' ORDER BY EVENT_NAME",
		}
		for _, charset := range []string{"sjis", "cp932", "big5", "gbk", "latin1", "utf8mb4", "ujis", "euckr"} {
			queries = append(queries, "SELECT HEX(charset_probe.f_"+charset+"())", "CALL charset_probe.p_"+charset+"()")
			queries = append(queries, "INSERT INTO charset_probe.t_"+charset+" VALUES ('trigger test'); SELECT HEX(value) FROM charset_probe.t_"+charset+"; TRUNCATE charset_probe.t_"+charset)
		}
		want := make([]string, len(queries))
		for i, query := range queries {
			want[i] = sql(t, query)
		}
		for _, mode := range []string{"per-database", "combined"} {
			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/encrypted=%t", mode, encrypted), func(t *testing.T) {
					env := map[string]string{
						"MARIADB_DATABASE": "charset_probe", "BACKUP_MODE": mode, "BACKUP_FILENAME_MODE": "fixed",
						"S3_PREFIX": fmt.Sprintf("mixed-charsets/%s/%t", mode, encrypted),
					}
					if encrypted {
						env["PASSPHRASE"] = "mixed charset test"
					}
					runBackupImage(t, env, "mariadb-backup-s3", "backup")
					sql(t, "DROP DATABASE charset_probe")
					if mode == "combined" {
						env["MARIADB_DATABASE"] = ""
					} else {
						sql(t, "CREATE DATABASE charset_probe CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
					}
					runBackupImage(t, env, "mariadb-backup-s3", "restore")
					for i, query := range queries {
						if got := sql(t, query); got != want[i] {
							t.Fatalf("charset restore changed %s:\ngot: %s\nwant: %s", query, got, want[i])
						}
					}
				})
			}
		}
	})
	t.Run("restore_line_break_names_and_empty_comment", func(t *testing.T) {
		sql(t, "CREATE DATABASE name_probe CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
		defer sql(t, "DROP DATABASE IF EXISTS name_probe")
		for _, table := range []string{"t\nx", "t\rx", "tx"} {
			sql(t, "CREATE TABLE name_probe.`"+table+"` (id INT); INSERT INTO name_probe.`"+table+"` VALUES (42)")
		}
		const namesQuery = "SELECT HEX(TABLE_NAME) FROM information_schema.TABLES WHERE TABLE_SCHEMA='name_probe' ORDER BY HEX(TABLE_NAME)"
		wantNames := sql(t, namesQuery)
		for _, mode := range []string{"per-database", "combined"} {
			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/encrypted=%t", mode, encrypted), func(t *testing.T) {
					env := map[string]string{"MARIADB_DATABASE": "name_probe", "BACKUP_MODE": mode, "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": fmt.Sprintf("line-break-names/%s/%t", mode, encrypted)}
					if encrypted {
						env["PASSPHRASE"] = "line break names test"
					}
					runBackupImage(t, env, "mariadb-backup-s3", "backup")
					// Native dumps omit COMMENT for this source. Restore must clear
					// the destination's old comment and recover every distinct name.
					sql(t, "DROP DATABASE name_probe; CREATE DATABASE name_probe CHARACTER SET latin1 COMMENT 'stale comment'")
					if mode == "combined" {
						env["MARIADB_DATABASE"] = ""
					}
					runBackupImage(t, env, "mariadb-backup-s3", "restore")
					if got := sql(t, namesQuery); got != wantNames {
						t.Fatalf("table names changed: %q, want %q", got, wantNames)
					}
					for _, table := range []string{"t\nx", "t\rx", "tx"} {
						if got := sql(t, "SELECT id FROM name_probe.`"+table+"`"); got != "42" {
							t.Fatalf("table %q data changed: %q", table, got)
						}
					}
					if got := sql(t, "SELECT HEX(SCHEMA_COMMENT) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='name_probe'"); got != "" {
						t.Fatalf("empty source comment was not restored: %q", got)
					}
				})
			}
		}
	})
	t.Run("restore_database_comments", func(t *testing.T) {
		sql(t, `CREATE DATABASE comment_probe CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'business data: it''s \\ 日本語; */ --\\';
			CREATE TABLE comment_probe.probe(id INT); INSERT INTO comment_probe.probe VALUES(11)`)
		defer sql(t, "DROP DATABASE IF EXISTS comment_probe")
		const commentQuery = "SELECT HEX(SCHEMA_COMMENT) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='comment_probe'"
		want := sql(t, commentQuery)
		for _, mode := range []string{"per-database", "combined"} {
			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/encrypted=%t", mode, encrypted), func(t *testing.T) {
					env := map[string]string{"MARIADB_DATABASE": "comment_probe", "BACKUP_MODE": mode, "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": fmt.Sprintf("comments/%s/%t", mode, encrypted)}
					if encrypted {
						env["PASSPHRASE"] = "comment test"
					}
					runBackupImage(t, env, "mariadb-backup-s3", "backup")
					if mode == "combined" {
						env["MARIADB_DATABASE"] = ""
					}
					for _, existing := range []bool{false, true} {
						for _, noBackslash := range []bool{false, true} {
							sql(t, "DROP DATABASE comment_probe")
							if existing {
								sql(t, "CREATE DATABASE comment_probe CHARACTER SET latin1 COMMENT 'old comment'")
							}
							originalMode := sql(t, "SELECT @@GLOBAL.sql_mode")
							if noBackslash {
								sql(t, "SET GLOBAL sql_mode='NO_BACKSLASH_ESCAPES'")
							}
							output, err := runImage(t, env, "mariadb-backup-s3", "restore")
							sql(t, "SET GLOBAL sql_mode='"+originalMode+"'")
							if err != nil {
								t.Fatalf("restore failed: %v\n%s", err, output)
							}
							if got := sql(t, commentQuery); got != want {
								t.Fatalf("comment changed: %s, want %s", got, want)
							}
							if got := sql(t, "SELECT id FROM comment_probe.probe"); got != "11" {
								t.Fatalf("data changed: %s", got)
							}
						}
					}
				})
			}
		}
	})
	if lowerCaseTableNames == 1 {
		t.Run("restore_database_aliases", func(t *testing.T) {
			for _, database := range []string{"alias_probe", "alias_other"} {
				sql(t, "CREATE DATABASE "+database+" CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'saved comment';"+
					"CREATE TABLE "+database+".probe(id INT PRIMARY KEY, value VARCHAR(20)) WITH SYSTEM VERSIONING;"+
					"INSERT INTO "+database+".probe VALUES(1,'before'); UPDATE "+database+".probe SET value='saved';"+
					"CREATE VIEW "+database+".v AS SELECT value FROM "+database+".probe;"+
					"CREATE FUNCTION "+database+".same_case(v VARCHAR(1)) RETURNS INT DETERMINISTIC RETURN v='A';"+
					"CREATE SEQUENCE "+database+".s START WITH 17")
				defer sql(t, "DROP DATABASE IF EXISTS "+database)
			}
			const query = "SELECT id,value,ROW_START,ROW_END FROM alias_probe.probe FOR SYSTEM_TIME ALL ORDER BY ROW_START"
			wantHistory := sql(t, query)
			for _, mode := range []string{"per-database", "combined"} {
				for _, encrypted := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/encrypted=%t", mode, encrypted), func(t *testing.T) {
						env := map[string]string{"MARIADB_DATABASE": "ALIAS_PROBE", "BACKUP_MODE": mode, "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": fmt.Sprintf("alias/%s/%t", mode, encrypted)}
						if mode == "combined" {
							env["MARIADB_DATABASE"], env["MARIADB_DATABASES"] = "", "ALIAS_PROBE,ALIAS_OTHER"
						}
						if encrypted {
							env["PASSPHRASE"] = "alias test"
						}
						runBackupImage(t, env, "mariadb-backup-s3", "backup")
						if mode == "combined" {
							env["MARIADB_DATABASES"] = ""
						}
						for _, existing := range []bool{false, true} {
							sql(t, "DROP DATABASE alias_probe")
							if mode == "combined" {
								// Both resolved databases must actually be restored; an
								// unchanged second database would hide a skipped import.
								sql(t, "UPDATE alias_other.probe SET value='changed'; ALTER DATABASE alias_other CHARACTER SET latin1 COMMENT 'old other'")
							}
							if existing {
								sql(t, "CREATE DATABASE alias_probe CHARACTER SET latin1 COMMENT 'old comment'; CREATE TABLE alias_probe.probe(id INT,value TEXT); INSERT INTO alias_probe.probe VALUES(99,'sentinel')")
								// Conflicting aliases must fail before creating/changing any SQL objects.
								env["RESTORE_DATABASE_DEFAULTS"] = `{"ALIAS_PROBE":{"character_set":"latin1","collation":"latin1_swedish_ci"},"alias_probe":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`
								output, err := runImage(t, env, "mariadb-backup-s3", "restore")
								if err == nil || !strings.Contains(output, "conflicting aliases") || !strings.Contains(output, "no restore SQL was executed") {
									t.Fatalf("conflicting aliases were not rejected: %v\n%s", err, output)
								}
								if got := sql(t, "SELECT id,value FROM alias_probe.probe"); got != "99\tsentinel" {
									t.Fatalf("failed preflight changed data: %s", got)
								}
								if got := sql(t, "SELECT DEFAULT_COLLATION_NAME,SCHEMA_COMMENT FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='alias_probe'"); got != "latin1_swedish_ci\told comment" {
									t.Fatalf("failed preflight changed metadata: %s", got)
								}
								if mode == "combined" {
									if got := sql(t, "SELECT value FROM alias_other.probe"); got != "changed" {
										t.Fatalf("failed preflight changed second database: %s", got)
									}
								}
							}
							// Test both native metadata alone and an override using a third spelling.
							env["RESTORE_DATABASE_DEFAULTS"] = ""
							if existing {
								env["RESTORE_DATABASE_DEFAULTS"] = `{"AlIaS_PrObE":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`
							}
							runBackupImage(t, env, "mariadb-backup-s3", "restore")
							if got := sql(t, query); got != wantHistory {
								t.Fatalf("alias restore changed history: %s, want %s", got, wantHistory)
							}
							for _, database := range []string{"alias_probe", "alias_other"} {
								if got := sql(t, "SELECT value FROM "+database+".v"); got != "saved" {
									t.Fatalf("view data changed: %s", got)
								}
								if got := sql(t, "SELECT "+database+".same_case('a')"); got != "0" {
									t.Fatalf("routine collation changed: %s", got)
								}
								if got := sql(t, "SELECT next_not_cached_value FROM "+database+".s"); got != "17" {
									t.Fatalf("sequence changed: %s", got)
								}
								if got := sql(t, "SELECT DEFAULT_COLLATION_NAME,SCHEMA_COMMENT FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='"+database+"'"); got != "utf8mb4_bin\tsaved comment" {
									t.Fatalf("database metadata changed: %s", got)
								}
							}
						}
					})
				}
			}
		})
	}

	if lowerCaseTableNames == 0 {
		t.Run("restore_across_name_comparison_settings", func(t *testing.T) {
			// A separate initialized server is essential: lower_case_table_names
			// cannot be changed dynamically. Share only the network namespace.
			destination := name + "-destination"
			t.Cleanup(func() { dockerCleanup(t, "rm", "-f", "-v", destination) })
			docker(t, ctx, "run", "-d", "--name", destination, "--network", "container:"+mariadb,
				"-e", "MARIADB_ROOT_PASSWORD=integration-password", envOr("MARIADB_TEST_IMAGE", "mariadb:11.4"),
				"--port=3307", "--lower-case-table-names=1")
			waitFor(t, ctx, "case-insensitive destination", func(ctx context.Context) error {
				_, err := exec.CommandContext(ctx, "docker", "exec", destination, "mariadb", "--host=127.0.0.1", "--port=3307", "--user=root", "--password=integration-password", "--execute=SELECT 1").CombinedOutput()
				return err
			})
			destinationSQL := func(query string) string {
				return strings.TrimSpace(docker(t, ctx, "exec", destination, "mariadb", "--host=127.0.0.1", "--port=3307", "--user=root", "--password=integration-password", "--batch", "--skip-column-names", "--execute", query))
			}
			sql(t, `CREATE DATABASE CaseAudit; CREATE DATABASE caseaudit;
				CREATE TABLE CaseAudit.probe(id INT); INSERT INTO CaseAudit.probe VALUES(11);
				CREATE TABLE caseaudit.probe(id INT); INSERT INTO caseaudit.probe VALUES(22);
				CREATE DATABASE table_case_audit;
				CREATE TABLE table_case_audit.T(id INT); INSERT INTO table_case_audit.T VALUES(11);
				CREATE TABLE table_case_audit.t(id INT); INSERT INTO table_case_audit.t VALUES(22);
				CREATE DATABASE safe_case_audit; CREATE TABLE safe_case_audit.MixedCase(id INT); INSERT INTO safe_case_audit.MixedCase VALUES(33)`)
			defer sql(t, "DROP DATABASE CaseAudit; DROP DATABASE caseaudit; DROP DATABASE table_case_audit; DROP DATABASE safe_case_audit")
			for _, tc := range []struct{ name, mode, database, databases, want string }{
				{"databases", "combined", "", "CaseAudit,caseaudit", "database name collision"},
				{"tables", "per-database", "table_case_audit", "", "table name collision"},
				{"combined_tables", "combined", "table_case_audit", "", "table name collision"},
				{"safe", "per-database", "safe_case_audit", "", ""},
			} {
				for _, encrypted := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/encrypted=%t", tc.name, encrypted), func(t *testing.T) {
						env := map[string]string{"MARIADB_DATABASE": tc.database, "MARIADB_DATABASES": tc.databases, "BACKUP_MODE": tc.mode, "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": fmt.Sprintf("name-comparison/%s/%t", tc.name, encrypted)}
						if encrypted {
							env["PASSPHRASE"] = "name collision test"
						}
						runBackupImage(t, env, "mariadb-backup-s3", "backup")
						if tc.mode == "combined" {
							env["MARIADB_DATABASE"], env["MARIADB_DATABASES"] = "", ""
						}
						// The same backup remains valid on the case-sensitive source.
						runBackupImage(t, env, "mariadb-backup-s3", "restore")
						if got := sql(t, "SELECT id FROM CaseAudit.probe UNION ALL SELECT id FROM caseaudit.probe UNION ALL SELECT id FROM table_case_audit.T UNION ALL SELECT id FROM table_case_audit.t"); got != "11\n22\n11\n22" {
							t.Fatalf("case-sensitive restore changed distinct objects: %s", got)
						}
						env["MARIADB_PORT"] = "3307"
						for _, existing := range []bool{false, true} {
							destinationSQL("DROP DATABASE IF EXISTS caseaudit; DROP DATABASE IF EXISTS table_case_audit; DROP DATABASE IF EXISTS safe_case_audit")
							if existing {
								destinationSQL("CREATE DATABASE caseaudit; CREATE TABLE caseaudit.probe(id INT); INSERT INTO caseaudit.probe VALUES(99); CREATE DATABASE table_case_audit; CREATE TABLE table_case_audit.t(id INT); INSERT INTO table_case_audit.t VALUES(99)")
							}
							output, err := runImage(t, env, "mariadb-backup-s3", "restore")
							if tc.want == "" {
								if err != nil {
									t.Fatalf("safe restore failed: %v\n%s", err, output)
								}
								if got := destinationSQL("SELECT id FROM safe_case_audit.mixedcase"); got != "33" {
									t.Fatal(got)
								}
								continue
							}
							if err == nil || !strings.Contains(output, tc.want) || !strings.Contains(output, "no restore SQL was executed") || strings.Contains(output, "Restore complete.") {
								t.Fatalf("unsafe restore was not rejected: %v\n%s", err, output)
							}
							if existing {
								if got := destinationSQL("SELECT id FROM caseaudit.probe UNION ALL SELECT id FROM table_case_audit.t"); got != "99\n99" {
									t.Fatalf("destination changed: %s", got)
								}
							} else if got := destinationSQL("SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME IN ('caseaudit','table_case_audit')"); got != "0" {
								t.Fatalf("restore created databases before rejecting: %s", got)
							}
						}
					})
				}
			}
		})
	}
	t.Run("restore_database_defaults", func(t *testing.T) {
		sql(t, "CREATE DATABASE collation_probe CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
		defer sql(t, "DROP DATABASE IF EXISTS collation_probe")
		sql(t, "CREATE FUNCTION collation_probe.same_case(v VARCHAR(1)) RETURNS INT DETERMINISTIC RETURN v='A'")
		sql(t, "ALTER DATABASE collation_probe COLLATE utf8mb4_general_ci; CREATE FUNCTION collation_probe.old_case(v VARCHAR(1)) RETURNS INT DETERMINISTIC RETURN v='A'; ALTER DATABASE collation_probe COLLATE utf8mb4_bin")
		verify := func(t *testing.T) {
			t.Helper()
			if got := sql(t, "SELECT collation_probe.same_case('a'), collation_probe.old_case('a')"); got != "0\t1" {
				t.Fatalf("routine comparison changed: %s", got)
			}
			if got := sql(t, "SELECT DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='collation_probe'"); got != "utf8mb4_bin" {
				t.Fatalf("database default changed: %s", got)
			}
			if got := sql(t, "SELECT ROUTINE_NAME, DATABASE_COLLATION FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA='collation_probe' ORDER BY ROUTINE_NAME"); got != "old_case\tutf8mb4_general_ci\nsame_case\tutf8mb4_bin" {
				t.Fatalf("routine defaults changed: %s", got)
			}
		}
		verify(t)
		for _, mode := range []string{"per-database", "combined"} {
			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/encrypted=%t", mode, encrypted), func(t *testing.T) {
					env := map[string]string{"MARIADB_DATABASE": "collation_probe", "BACKUP_MODE": mode, "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": fmt.Sprintf("defaults/%s/%t", mode, encrypted)}
					if encrypted {
						env["PASSPHRASE"] = "defaults test"
					}
					runBackupImage(t, env, "mariadb-backup-s3", "backup")
					if mode == "combined" {
						env["MARIADB_DATABASE"] = ""
					}
					for _, existing := range []bool{false, true} {
						sql(t, "DROP DATABASE collation_probe")
						if existing {
							sql(t, "CREATE DATABASE collation_probe CHARACTER SET latin1 COLLATE latin1_swedish_ci")
						}
						runBackupImage(t, env, "mariadb-backup-s3", "restore")
						verify(t)
					}
				})
			}
		}
		t.Run("uca1400", func(t *testing.T) {
			sql(t, "ALTER DATABASE collation_probe COLLATE utf8mb4_uca1400_ai_ci")
			env := map[string]string{"MARIADB_DATABASE": "collation_probe", "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": "uca1400-defaults"}
			runBackupImage(t, env, "mariadb-backup-s3", "backup")
			sql(t, "DROP DATABASE collation_probe")
			runBackupImage(t, env, "mariadb-backup-s3", "restore")
			if got := sql(t, "SELECT DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='collation_probe'"); got != "utf8mb4_uca1400_ai_ci" {
				t.Fatal(got)
			}
			sql(t, "ALTER DATABASE collation_probe COLLATE utf8mb4_bin")
			verify(t)
		})
		for _, external := range []string{"no_metadata", "native", "plain_create"} {
			metadata := external != "no_metadata"
			t.Run("external/"+external, func(t *testing.T) {
				args := []string{"exec", mariadb, "mariadb-dump", "--user=root", "--password=integration-password", "--routines", "--default-character-set=utf8mb4"}
				if metadata {
					args = append(args, "--databases")
				}
				args = append(args, "collation_probe")
				dump := docker(t, ctx, args...)
				if external == "plain_create" {
					dump = strings.ReplaceAll(dump, "/*!32312 IF NOT EXISTS*/", "")
				}
				prefix := "external-defaults/" + external
				_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(prefix + "/collation_probe_latest.sql"), Body: strings.NewReader(dump)})
				if err != nil {
					t.Fatal(err)
				}
				env := map[string]string{"MARIADB_DATABASE": "collation_probe", "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": prefix}
				if !metadata {
					env["RESTORE_DATABASE_DEFAULTS"] = `{"collation_probe":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`
				}
				sql(t, "DROP DATABASE collation_probe; CREATE DATABASE collation_probe CHARACTER SET latin1 COLLATE latin1_swedish_ci")
				runBackupImage(t, env, "mariadb-backup-s3", "restore")
				verify(t)
			})
		}
	})
	t.Run("restore_sql_modes_and_routines", func(t *testing.T) {
		sql(t, "CREATE DATABASE mode_probe")
		defer sql(t, "DROP DATABASE mode_probe")
		sql(t, "CREATE TABLE mode_probe.`tail\\` (value TEXT); INSERT INTO mode_probe.`tail\\` VALUES (UNHEX('61273B225C0A62'))")
		sql(t, "SET SESSION sql_mode='NO_BACKSLASH_ESCAPES'; CREATE PROCEDURE mode_probe.no_escape() SELECT 'tail\\'")
		sql(t, "SET SESSION sql_mode='ANSI_QUOTES'; CREATE PROCEDURE mode_probe.ansi_quote() SELECT 1 AS \"tail\\\"")
		// A quoted comment terminator and delimiter inside a large executable
		// routine comment must not terminate the statement during preflight.
		sql(t, "CREATE PROCEDURE mode_probe.comment_end() SELECT LENGTH('*/;;"+strings.Repeat("x", 70000)+"')")
		env := map[string]string{"MARIADB_DATABASE": "mode_probe", "S3_PREFIX": "mode-probe", "BACKUP_FILENAME_MODE": "fixed"}
		runBackupImage(t, env, "mariadb-backup-s3", "backup")
		sql(t, "TRUNCATE mode_probe.`tail\\`; DROP PROCEDURE mode_probe.no_escape; DROP PROCEDURE mode_probe.ansi_quote; DROP PROCEDURE mode_probe.comment_end")
		runBackupImage(t, env, "mariadb-backup-s3", "restore")
		for query, want := range map[string]string{
			"SELECT HEX(value) FROM mode_probe.`tail\\`": "61273B225C0A62",
			"CALL mode_probe.no_escape()":                "tail\\\\", // Batch output escapes the backslash.
			"CALL mode_probe.ansi_quote()":               "1",
			"CALL mode_probe.comment_end()":              "70004",
		} {
			if got := sql(t, query); got != want {
				t.Fatalf("restored SQL mode query %q = %q, want %q", query, got, want)
			}
		}
	})
	t.Run("restore_packet_preflight", func(t *testing.T) {
		originalLimit := sql(t, "SELECT @@GLOBAL.max_allowed_packet")
		defer sql(t, "SET GLOBAL max_allowed_packet="+originalLimit)
		sql(t, "CREATE DATABASE packet_probe")
		defer sql(t, "DROP DATABASE packet_probe")
		for _, mode := range []string{"per-database", "combined"} {
			for _, encrypted := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/encrypted=%t", mode, encrypted), func(t *testing.T) {
					sql(t, "SET GLOBAL max_allowed_packet=16777216")
					sql(t, "DROP TABLE IF EXISTS packet_probe.items; CREATE TABLE packet_probe.items(id INT PRIMARY KEY, payload LONGBLOB); INSERT INTO packet_probe.items VALUES(1,REPEAT('x',10485760))")
					env := map[string]string{
						"MARIADB_DATABASE": "packet_probe", "BACKUP_MODE": mode,
						"S3_PREFIX": fmt.Sprintf("packet-probe/%s/%t", mode, encrypted), "BACKUP_FILENAME_MODE": "fixed",
					}
					if encrypted {
						env["PASSPHRASE"] = "packet test passphrase"
					}
					runBackupImage(t, env, "mariadb-backup-s3", "backup")
					if mode == "combined" {
						env["MARIADB_DATABASE"] = ""
					}
					// A post-backup row proves rejection happens before DROP/CREATE.
					sql(t, "INSERT INTO packet_probe.items VALUES(2,'must survive failed restore')")
					checksum := "SELECT id, OCTET_LENGTH(payload), MD5(payload) FROM packet_probe.items ORDER BY id"
					want := sql(t, checksum)
					output, err := runImage(t, env, "mariadb-backup-s3", "restore")
					if err == nil || !strings.Contains(output, "max_allowed_packet=16777216") || !strings.Contains(output, "no restore SQL was executed") {
						t.Fatalf("oversized restore was not rejected before execution: %v\n%s", err, output)
					}
					if got := sql(t, checksum); got != want {
						t.Fatalf("preflight changed original data: got %q, want %q", got, want)
					}
					sql(t, "SET GLOBAL max_allowed_packet=67108864")
					runBackupImage(t, env, "mariadb-backup-s3", "restore")
					wantRestored := strings.Split(want, "\n")[0]
					if got := sql(t, checksum); got != wantRestored {
						t.Fatalf("restore after increasing limit changed data: got %q, want %q", got, wantRestored)
					}
				})
			}
		}
	})

	t.Run("discovery_preserves_similar_business_names", func(t *testing.T) {
		businessNames := []string{"mýsql", "sýs"}
		if lowerCaseTableNames == 0 {
			businessNames = append(businessNames, "SYS")
		}
		for _, database := range businessNames {
			sql(t, "CREATE DATABASE `"+database+"`; CREATE TABLE `"+database+"`.discovery_probe (id INT); INSERT INTO `"+database+"`.discovery_probe VALUES (42)")
			defer sql(t, "DROP DATABASE `"+database+"`")
		}
		for _, mode := range []string{"per-database", "combined"} {
			t.Run(mode, func(t *testing.T) {
				prefix := "discovery-similar/" + mode
				env := map[string]string{
					"MARIADB_DATABASE": "", "MARIADB_BACKUP_ALL": "true",
					"BACKUP_MODE": mode, "BACKUP_FILENAME_MODE": "fixed", "S3_PREFIX": prefix,
				}
				runBackupImage(t, env, "mariadb-backup-s3", "backup")
				keys := listKeys(t, prefix+"/")
				if mode == "per-database" {
					wantKeys := []string{prefix + "/app_latest.sql", prefix + "/analytics_latest.sql", prefix + "/excluded_latest.sql"}
					for _, database := range businessNames {
						key := prefix + "/" + url.PathEscape(database) + "_latest.sql"
						wantKeys = append(wantKeys, key)
						dump := readDump(t, key)
						if !strings.Contains(strings.Join(strings.Fields(dump), " "), "INSERT INTO `discovery_probe` VALUES (42);") {
							t.Fatalf("business database %q was not dumped:\n%s", database, dump)
						}
					}
					sort.Strings(wantKeys)
					if strings.Join(keys, "\n") != strings.Join(wantKeys, "\n") {
						t.Fatalf("discovered objects = %v, want %v", keys, wantKeys)
					}
				} else {
					if len(keys) != 1 {
						t.Fatalf("expected one combined dump, got %v", keys)
					}
					dump := readDump(t, keys[0])
					for _, database := range businessNames {
						if !strings.Contains(dump, "USE `"+database+"`;") {
							t.Fatalf("combined dump omitted business database %q", database)
						}
					}
					for _, database := range []string{"mysql", "sys", "information_schema", "performance_schema"} {
						if strings.Contains(dump, "USE `"+database+"`;") {
							t.Fatalf("combined dump included system database %q", database)
						}
					}
				}
			})
		}
	})

	if lowerCaseTableNames == 0 {
		t.Run("discovery_reports_mysql_client_failure", func(t *testing.T) {
			// MariaDB 11.4's dump client treats MYSQL as the system schema
			// and tries to dump nonexistent log tables even on a case-sensitive
			// server. Discovery must report this failure, not silently skip it.
			sql(t, "CREATE DATABASE MYSQL; CREATE TABLE MYSQL.business_data (id INT)")
			defer sql(t, "DROP DATABASE MYSQL")
			for _, mode := range []string{"per-database", "combined"} {
				env := map[string]string{
					"MARIADB_DATABASE": "", "MARIADB_BACKUP_ALL": "true",
					"MARIADB_DATABASES_EXCLUDE": "app,analytics,excluded",
					"BACKUP_MODE":               mode, "S3_PREFIX": "mysql-client-failure/" + mode,
				}
				output, err := runImage(t, env, "mariadb-backup-s3", "backup")
				if err == nil || !strings.Contains(output, "MYSQL.general_log") || !strings.Contains(output, "Backup failed:") {
					t.Fatalf("dump client limitation was not reported: %v\n%s", err, output)
				}
				if keys := listKeys(t, env["S3_PREFIX"]+"/"); len(keys) != 0 {
					t.Fatalf("failed dump was published: %v", keys)
				}
			}
		})
	}

	t.Run("history_metadata_scan_scope", func(t *testing.T) {
		// Inspect the actual query issued by the CLI, so the regression check
		// measures the server's plan rather than duplicating the query builder.
		sql(t, "SET GLOBAL log_output='TABLE'; TRUNCATE TABLE mysql.general_log; SET GLOBAL general_log=ON")
		defer sql(t, "SET GLOBAL general_log=OFF")
		selected := "app,analytics"
		if lowerCaseTableNames != 0 {
			selected = "APP,ANALYTICS"
		}
		runBackupImage(t, map[string]string{
			"MARIADB_DATABASES": selected, "S3_PREFIX": "history-scan-scope",
			"MARIADB_DUMP_EXTRA_OPTS": "--ignore-table=app.unused",
		}, "mariadb-backup-s3", "backup")
		sql(t, "SET GLOBAL general_log=OFF")
		queryHex := sql(t, "SELECT HEX(argument) FROM mysql.general_log WHERE command_type='Query' AND argument LIKE 'SELECT DISTINCT HEX(TABLE_SCHEMA)%' ORDER BY event_time DESC LIMIT 1")
		query, err := hex.DecodeString(queryHex)
		if err != nil || len(query) == 0 {
			t.Fatalf("could not capture history query: %v (%q)", err, queryHex)
		}
		plan := sql(t, "EXPLAIN "+string(query))
		if strings.Contains(plan, "Scanned all databases") || strings.Count(plan, "Scanned 1 database") != 2 {
			t.Fatalf("history metadata scan was not limited to the two selected databases:\n%s", plan)
		}
	})

	t.Run("transaction_precise_history_preflight", func(t *testing.T) {
		sql(t, `
			CREATE DATABASE history_check;
			CREATE TABLE history_check.history (
				id INT PRIMARY KEY, value VARCHAR(32),
				rs BIGINT UNSIGNED GENERATED ALWAYS AS ROW START,
				re BIGINT UNSIGNED GENERATED ALWAYS AS ROW END,
				PERIOD FOR SYSTEM_TIME(rs,re)
			) ENGINE=InnoDB WITH SYSTEM VERSIONING;
			INSERT INTO history_check.history(id,value) VALUES(1,'first');
			UPDATE history_check.history SET value='second' WHERE id=1;
			CREATE TABLE history_check.ordinary (id INT PRIMARY KEY, n BIGINT);
			CREATE TABLE history_check.timestamp_history (
				id INT PRIMARY KEY, n BIGINT,
				rs TIMESTAMP(6) GENERATED ALWAYS AS ROW START,
				re TIMESTAMP(6) GENERATED ALWAYS AS ROW END,
				PERIOD FOR SYSTEM_TIME(rs,re)
			) WITH SYSTEM VERSIONING;
		`)
		defer sql(t, "DROP DATABASE history_check")
		for _, mode := range []string{"per-database", "combined"} {
			t.Run(mode, func(t *testing.T) {
				env := map[string]string{
					"MARIADB_DATABASES": "app,history_check", "BACKUP_MODE": mode,
					"S3_PREFIX": "history-preflight/" + mode, "BACKUP_FILENAME_MODE": "fixed", "BACKUP_KEEP_DAYS": "1",
				}
				for _, option := range []string{"", "--ignore-table-data=history_check.history"} {
					env["MARIADB_DUMP_EXTRA_OPTS"] = option
					output, err := runImage(t, env, "mariadb-backup-s3", "backup")
					if err == nil || !strings.Contains(output, `unsupported transaction-precise system-versioned tables: "history_check"."history"`) || !strings.Contains(output, "no dumps were started") {
						t.Fatalf("preflight did not reject transaction history: %v\n%s", err, output)
					}
					if keys := listKeys(t, env["S3_PREFIX"]+"/"); len(keys) != 0 {
						t.Fatalf("preflight failure published objects: %v", keys)
					}
				}
				// A full table exclusion succeeds; ordinary BIGINT and explicit
				// timestamp period columns must not trigger a false positive.
				env["MARIADB_DUMP_EXTRA_OPTS"] = "--ignore-table=history_check.history"
				runBackupImage(t, env, "mariadb-backup-s3", "backup")
			})
		}
		// Automatic discovery must apply database exclusions before preflight.
		runBackupImage(t, map[string]string{
			"MARIADB_DATABASE": "", "MARIADB_BACKUP_ALL": "true", "MARIADB_DATABASES_EXCLUDE": "history_check",
			"S3_PREFIX": "history-preflight/excluded",
		}, "mariadb-backup-s3", "backup")
	})

	t.Run("history_many_databases", func(t *testing.T) {
		names := make([]string, 400)
		var create, drop strings.Builder
		for i := range names {
			names[i] = fmt.Sprintf("many_%04d", i)
			fmt.Fprintf(&create, "CREATE DATABASE `%s`;", names[i])
			fmt.Fprintf(&drop, "DROP DATABASE IF EXISTS `%s`;", names[i])
		}
		defer sql(t, drop.String())
		sql(t, create.String())
		sql(t, "CREATE TABLE many_0399.history (id INT, rs BIGINT UNSIGNED GENERATED ALWAYS AS ROW START, re BIGINT UNSIGNED GENERATED ALWAYS AS ROW END, PERIOD FOR SYSTEM_TIME(rs,re)) ENGINE=InnoDB WITH SYSTEM VERSIONING")
		for _, mode := range []string{"per-database", "combined"} {
			env := map[string]string{"MARIADB_DATABASE": "", "MARIADB_DATABASES": strings.Join(names, ","), "BACKUP_MODE": mode, "S3_PREFIX": "history-many/" + mode}
			output, err := runImage(t, env, "mariadb-backup-s3", "backup")
			if err == nil || !strings.Contains(output, `unsupported transaction-precise system-versioned tables: "many_0399"."history"`) || !strings.Contains(output, "no dumps were started") {
				t.Fatalf("large preflight failed: %v\n%s", err, output)
			}
			if keys := listKeys(t, env["S3_PREFIX"]+"/"); len(keys) != 0 {
				t.Fatalf("large preflight published objects: %v", keys)
			}
		}
	})

	t.Run("history_name_matching", func(t *testing.T) {
		sql(t, `
			CREATE DATABASE case_scope;
			CREATE TABLE case_scope.history (
				id INT PRIMARY KEY,
				rs BIGINT UNSIGNED GENERATED ALWAYS AS ROW START,
				re BIGINT UNSIGNED GENERATED ALWAYS AS ROW END,
				PERIOD FOR SYSTEM_TIME(rs,re)
			) ENGINE=InnoDB WITH SYSTEM VERSIONING;
			CREATE TABLE case_scope.é LIKE case_scope.history;
		`)
		defer sql(t, "DROP DATABASE case_scope")
		if lowerCaseTableNames == 0 {
			sql(t, "CREATE DATABASE CASE_SCOPE; CREATE TABLE CASE_SCOPE.ordinary (id INT)")
			defer sql(t, "DROP DATABASE CASE_SCOPE")
		}
		for _, mode := range []string{"per-database", "combined"} {
			t.Run(mode, func(t *testing.T) {
				env := map[string]string{
					"MARIADB_DATABASE": "CASE_SCOPE", "BACKUP_MODE": mode,
					"S3_PREFIX": "case-scope/" + mode, "BACKUP_FILENAME_MODE": "fixed",
				}
				output, err := runImage(t, env, "mariadb-backup-s3", "backup")
				if lowerCaseTableNames == 0 {
					if err != nil {
						t.Fatalf("unselected lowercase database blocked backup: %v\n%s", err, output)
					}
					if keys := listKeys(t, env["S3_PREFIX"]+"/"); len(keys) != 1 {
						t.Fatalf("expected one backup of uppercase database: %v", keys)
					}
				} else if err == nil || !strings.Contains(output, `"case_scope"."history"`) {
					t.Fatalf("uppercase selector bypassed history check on case-insensitive server: %v\n%s", err, output)
				}
				for i, tc := range []struct {
					opts string
					ok   bool
				}{
					{"--ignore-table=case_scope.history --ignore-table=case_scope.é", true},
					{"--ignore-table=CASE_SCOPE.HISTORY --ignore-table=CASE_SCOPE.é", true},
					// The client's ignore hash folds raw latin1 bytes, not
					// Unicode characters; strings.EqualFold would get this wrong.
					{"--ignore-table=CASE_SCOPE.HISTORY --ignore-table=case_scope.É", false},
				} {
					env["MARIADB_DATABASE"], env["MARIADB_DUMP_EXTRA_OPTS"] = "case_scope", tc.opts
					env["S3_PREFIX"] = fmt.Sprintf("case-ignore/%s/%d", mode, i)
					// Check the real client too, so a client upgrade cannot silently
					// change the contract the preflight must reproduce.
					args := []string{"mariadb-dump", "--no-defaults", "--host=127.0.0.1", "--user=root", "--password=integration-password", "--default-character-set=utf8mb4", "--dump-history"}
					args = append(args, strings.Fields(tc.opts)...)
					args = append(args, "--", "case_scope")
					output, err = runImage(t, env, args...)
					if (err == nil) != tc.ok {
						t.Fatalf("client exclusion semantics changed for %q: %v\n%s", tc.opts, err, output)
					}
					output, err = runImage(t, env, "mariadb-backup-s3", "backup")
					if (err == nil) != tc.ok {
						t.Fatalf("preflight disagrees with client for %q: %v\n%s", tc.opts, err, output)
					}
					if !tc.ok {
						if !strings.Contains(output, "no dumps were started") || len(listKeys(t, env["S3_PREFIX"]+"/")) != 0 {
							t.Fatalf("unsupported history was not rejected before upload: %s", output)
						}
					}
				}
			})
		}
		if lowerCaseTableNames == 0 {
			// Explicitly excluding case_scope must still allow CASE_SCOPE.
			runBackupImage(t, map[string]string{
				"MARIADB_DATABASE": "", "MARIADB_BACKUP_ALL": "true", "MARIADB_DATABASES_EXCLUDE": "case_scope",
				"S3_PREFIX": "case-scope/excluded",
			}, "mariadb-backup-s3", "backup")
		}
	})
	if lowerCaseTableNames != 0 {
		t.Run("history_unicode_database_alias", func(t *testing.T) {
			sql(t, `CREATE DATABASE case_é;
				CREATE TABLE case_é.history (id INT,
				rs BIGINT UNSIGNED GENERATED ALWAYS AS ROW START,
				re BIGINT UNSIGNED GENERATED ALWAYS AS ROW END,
				PERIOD FOR SYSTEM_TIME(rs,re)) ENGINE=InnoDB WITH SYSTEM VERSIONING`)
			defer sql(t, "DROP DATABASE case_é")
			for _, ignored := range []string{"case_é.history", "case_É.history"} {
				env := map[string]string{
					"MARIADB_DATABASE": "case_É", "S3_PREFIX": "unicode-alias",
					"MARIADB_DUMP_EXTRA_OPTS": "--ignore-table=" + ignored,
				}
				output, err := runImage(t, env, "mariadb-dump", "--no-defaults", "--host=127.0.0.1", "--user=root", "--password=integration-password", "--default-character-set=utf8mb4", "--dump-history", "--ignore-table="+ignored, "--", "case_É")
				wantSuccess := ignored == "case_É.history"
				if (err == nil) != wantSuccess {
					t.Fatalf("client database spelling semantics changed: %v\n%s", err, output)
				}
				output, err = runImage(t, env, "mariadb-backup-s3", "backup")
				if (err == nil) != wantSuccess || (!wantSuccess && !strings.Contains(output, "no dumps were started")) {
					t.Fatalf("preflight disagrees with client for database alias: %v\n%s", err, output)
				}
			}
		})
		return // The full restore suite runs once on the default case-sensitive server.
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
	if !t.Run("timestamp_distinct_objects", func(t *testing.T) {
		env := map[string]string{"S3_PREFIX": "distinct"}
		// Two independent CLI invocations must never overwrite the same key.
		runBackupImage(t, env, "sh", "-c", "sh backup.sh && sh backup.sh")
		keys := listKeys(t, "distinct/app/")
		if len(keys) != 2 {
			t.Fatalf("expected two distinct backup objects, got %v", keys)
		}
		id := strings.TrimSuffix(strings.TrimPrefix(keys[0], "distinct/app/"), ".sql")
		sql(t, "DROP DATABASE app; CREATE DATABASE app")
		runBackupImage(t, env, "mariadb-backup-s3", "restore", id)
		assertValue(t, "original")
		assertSchema(t)
	}) {
		return
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
		assertHistory(t)
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
