package tests

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestRestorePacketPreflightFailuresDoNotImportSQL(t *testing.T) {
	for _, tc := range []struct {
		name, variable, value, want string
	}{
		{"query_failure", "FAKE_PACKET_CHECK_FAILURE", "true", "could not check restore packet limit"},
		{"invalid_limit", "FAKE_PACKET_LIMIT", "invalid", "invalid restore packet limit result"},
		{"zero_limit", "FAKE_PACKET_LIMIT", "0", "invalid restore packet limit result"},
		{"invalid_mode", "FAKE_SQL_MODE_HEX", "not-hex", "invalid restore packet limit result"},
		{"oversize_sql", "FAKE_PACKET_LIMIT", "1024", "max_allowed_packet=1024"},
	} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/encrypted=%t", tc.name, encrypted), func(t *testing.T) {
				f := newFixture(t)
				f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = "app", "fixed"
				f.env[tc.variable] = tc.value
				if encrypted {
					f.env["PASSPHRASE"] = "test"
				}
				f.s3.downloadBody = "-- SQL\nINSERT INTO t VALUES ('" + strings.Repeat("secret", 1000) + "');\n"
				output := f.run(false, "restore")
				if !strings.Contains(output, tc.want) || !strings.Contains(output, "no restore SQL was executed") || strings.Contains(output, "secret") || strings.Contains(output, "Restore complete") {
					t.Fatalf("unexpected preflight output: %s", output)
				}
				assertEqual(t, len(f.processes("mariadb")), 1)
				assertEqual(t, len(f.databaseCalls("mariadb")), 0)
			})
		}
	}
}

func TestRestoreCharsetPreflightFailuresDoNotImportSQL(t *testing.T) {
	for _, sql := range []string{
		"SET character_set_client=@unknown;",
		"SET NAMES unsupported_charset;",
		"SET NAMES utf16;",
		"SET character_set_client=CONCAT('s','jis');",
		"SET NAMES sjis;\nDELIMITER ;;\nCREATE PROCEDURE p() SELECT '\x95\\;;" + strings.Repeat("x", 2000) + "';;\n",
	} {
		f := newFixture(t)
		f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = "app", "fixed"
		f.env["FAKE_PACKET_LIMIT"] = "1024"
		f.s3.downloadBody = "-- SQL\n" + sql
		output := f.run(false, "restore")
		if !strings.Contains(output, "no restore SQL was executed") {
			t.Fatalf("charset preflight did not reject SQL: %s", output)
		}
		assertEqual(t, len(f.databaseCalls("mariadb")), 0)
	}
}

func TestRestoreDatabaseDefaultsFailuresDoNotImportSQL(t *testing.T) {
	for _, tc := range []struct{ name, sql, explicit, variable, want string }{
		{name: "missing", sql: "CREATE TABLE t(id int);", want: "missing source database defaults"},
		{name: "unavailable", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE unknown_collation;", want: "unavailable"},
		{name: "conflicting", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;", explicit: `{"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`, want: "conflicts"},
		{name: "query_failure", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;", variable: "FAKE_COLLATION_FAILURE", want: "could not check destination collations"},
	} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/encrypted=%t", tc.name, encrypted), func(t *testing.T) {
				f := newFixture(t)
				f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = "app", "fixed"
				f.env["RESTORE_DATABASE_DEFAULTS"] = tc.explicit
				if tc.variable != "" {
					f.env[tc.variable] = "true"
				}
				if encrypted {
					f.env["PASSPHRASE"] = "test"
				}
				f.s3.downloadBody = "-- SQL\n" + tc.sql
				out := f.run(false, "restore")
				if !strings.Contains(out, tc.want) || !strings.Contains(out, "no restore SQL was executed") {
					t.Fatal(out)
				}
				assertEqual(t, len(f.databaseCalls("mariadb")), 0)
			})
		}
	}
}

func TestExternalRestoreWithExplicitDatabaseDefaults(t *testing.T) {
	for _, combined := range []bool{false, true} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("combined=%t/encrypted=%t", combined, encrypted), func(t *testing.T) {
				f := newFixture(t)
				f.env["BACKUP_FILENAME_MODE"] = "fixed"
				f.s3.downloadBody = "-- SQL external\nCREATE TABLE t(id int);\n"
				f.env["RESTORE_DATABASE_DEFAULTS"] = `{"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`
				if combined {
					f.env["BACKUP_MODE"] = "combined"
					f.s3.downloadBody = "USE `app`;\n" + f.s3.downloadBody
				} else {
					f.env["MARIADB_DATABASE"] = "app"
				}
				if encrypted {
					f.env["PASSPHRASE"] = "test"
				}
				f.run(true, "restore")
				payload, err := os.ReadFile(f.env["FAKE_RESTORE_INPUT"])
				if err != nil {
					t.Fatal(err)
				}
				sql := string(payload)
				if !strings.HasPrefix(sql, "CREATE DATABASE IF NOT EXISTS `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;\nALTER DATABASE `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;\n") || !strings.HasSuffix(sql, f.s3.downloadBody) {
					t.Fatal(sql)
				}
			})
		}
	}
}

func TestRestoreNamePreflightDoesNotImportSQL(t *testing.T) {
	for _, tc := range []struct{ name, sql, mode, want string }{
		{"databases", "CREATE DATABASE CaseAudit CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; CREATE DATABASE caseaudit CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;", "1", "database name collision"},
		{"tables", "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; USE app; CREATE TABLE T(id INT); CREATE TABLE t(id INT);", "1", "table name collision"},
		{"unicode", "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; USE app; CREATE TABLE `É`(id INT);", "2", "cannot safely compare non-ASCII"},
		{"invalid_setting", "SELECT 1;", "invalid", "invalid restore name-comparison result"},
		{"unknown_setting", "SELECT 1;", "3", "invalid restore name-comparison result"},
	} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/encrypted=%t", tc.name, encrypted), func(t *testing.T) {
				f := newFixture(t)
				f.env["BACKUP_MODE"], f.env["BACKUP_FILENAME_MODE"] = "combined", "fixed"
				f.env["FAKE_LOWER_CASE_TABLE_NAMES"] = tc.mode
				if encrypted {
					f.env["PASSPHRASE"] = "test"
				}
				f.s3.downloadBody = tc.sql
				output := f.run(false, "restore")
				if !strings.Contains(output, tc.want) || !strings.Contains(output, "no restore SQL was executed") || strings.Contains(output, "Restore complete") {
					t.Fatal(output)
				}
				assertEqual(t, len(f.databaseCalls("mariadb")), 0)
			})
		}
	}
}

func TestRestoreCommentsWithExplicitDefaults(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = "app", "fixed"
	f.env["RESTORE_DATABASE_DEFAULTS"] = `{"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`
	f.s3.downloadBody = "CREATE DATABASE app COMMENT 'business data'; USE app; CREATE TABLE t(id INT);\n"
	f.run(true, "restore")
	payload, err := os.ReadFile(f.env["FAKE_RESTORE_INPUT"])
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE DATABASE IF NOT EXISTS `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'business data';",
		"ALTER DATABASE `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'business data';",
	} {
		if !strings.Contains(string(payload), statement) {
			t.Fatalf("missing comment in %s: %s", statement, payload)
		}
	}
}

func TestRestoreCommentEndingInBackslashWithNoBackslashEscapes(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = "app", "fixed"
	// HEX('NO_BACKSLASH_ESCAPES'), as returned by the destination preflight.
	f.env["FAKE_SQL_MODE_HEX"] = "4E4F5F4241434B534C4153485F45534341504553"
	f.s3.downloadBody = "SET SQL_MODE=''; CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'tail\\\\'; USE app;\n"
	f.run(true, "restore")
	payload, err := os.ReadFile(f.env["FAKE_RESTORE_INPUT"])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "ALTER DATABASE `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'tail\\';") {
		t.Fatalf("comment was not quoted for destination SQL mode: %s", payload)
	}
}
