package backup

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestoreDatabaseDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mariadb"), []byte("#!/bin/sh\nprintf 'utf8mb4_bin\\tutf8mb4\\nutf8mb4_general_ci\\tutf8mb4\\nlatin1_swedish_ci\\tlatin1\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	for _, tc := range []struct {
		name, sql, explicit, want string
		combined                  bool
	}{
		{name: "native", sql: "CREATE DATABASE /*!32312 IF NOT EXISTS*/ `app` /*!40100 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin */; USE `app`;"},
		{name: "external", sql: "create schema app default charset=utf8mb4 collate='utf8mb4_bin';"},
		{name: "explicit", sql: "CREATE TABLE t(id int);", explicit: `{"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`},
		{name: "missing", sql: "CREATE TABLE t(id int);", want: "missing source database defaults"},
		{name: "partial", sql: "CREATE DATABASE app CHARACTER SET utf8mb4;", want: "missing source database defaults"},
		{name: "partial_explicit", sql: "CREATE DATABASE app CHARACTER SET utf8mb4;", explicit: `{"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`},
		{name: "conflict", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;", explicit: `{"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`, want: "conflicts"},
		{name: "unavailable", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE unknown_collation;", want: "unavailable"},
		{name: "wrong_pair", sql: "CREATE DATABASE app CHARACTER SET latin1 COLLATE utf8mb4_bin;", want: "does not belong"},
		{name: "other_database", sql: "CREATE DATABASE other CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;", want: "differs"},
		{name: "combined_unknown", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; USE other;", combined: true, want: "missing source database defaults"},
		{name: "missing_selection", sql: "SELECT 1;", combined: true, want: "no database selection"},
		{name: "invalid_json", explicit: `{"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin","extra":1}}`, want: "invalid RESTORE_DATABASE_DEFAULTS"},
		{name: "incomplete_json", explicit: `{"app":{"collation":"utf8mb4_bin"}}`, want: "requires character_set"},
		{name: "unused_override", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;", explicit: `{"other":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`, want: "unused"},
		{name: "drop_database", sql: "DROP DATABASE app;", want: "DROP DATABASE"},
		{name: "replace_database", sql: "CREATE OR REPLACE DATABASE app;", want: "CREATE OR REPLACE DATABASE"},
		{name: "duplicate", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;", want: "conflicting"},
		{name: "routine_override", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; ALTER DATABASE app CHARACTER SET latin1 COLLATE latin1_swedish_ci; CREATE PROCEDURE p() SELECT 1; ALTER DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;"},
		{name: "unsupported_routine_collation", sql: "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin; ALTER DATABASE app COLLATE missing_collation;", want: "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Database: "app", RestoreDatabaseDefaults: tc.explicit}
			if tc.combined {
				c.BackupMode = "combined"
				c.Database = ""
			}
			d, err := newRestoreDefaults(c)
			if err == nil {
				err = scanDumpPackets(context.Background(), strings.NewReader(tc.sql), 1<<20, "", nil, d.inspect)
			}
			var prefix string
			if err == nil {
				prefix, err = d.prepare(context.Background(), &App{}, commandRunner{stderr: io.Discard}, 1<<20)
			}
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("got %v, want %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			comment := ""
			if _, declared := d.declared["app"]; declared {
				comment = " COMMENT ''"
			}
			if !strings.Contains(prefix, "ALTER DATABASE `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"+comment+";\n") {
				t.Fatal(prefix)
			}
		})
	}
}

func TestRestoreDatabaseMetadataPreservesIdentifiers(t *testing.T) {
	const name = "CaSe`日本語"
	d, _ := newRestoreDefaults(Config{Database: name})
	sql := "-- ordinary comment\nCREATE DATABASE /*!32312 IF NOT EXISTS*/ `CaSe``日本語` /*!40100 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin */;\nUSE `CaSe``日本語`;\nDELIMITER ;;\nCREATE PROCEDURE p() BEGIN SELECT 'USE `other`; CREATE DATABASE bogus;'; END;;\nDELIMITER ;\n"
	if err := scanDumpPackets(context.Background(), strings.NewReader(sql), 1<<20, "", nil, d.inspect); err != nil {
		t.Fatal(err)
	}
	if len(d.used) != 1 || !d.used[name] || d.declared[name].Collation != "utf8mb4_bin" {
		t.Fatalf("metadata changed: %#v", d)
	}
	if quoteDatabase(name) != "`CaSe``日本語`" {
		t.Fatal("unsafe identifier quoting")
	}
}

func TestDatabaseCreateInsertionOffsets(t *testing.T) {
	sql := "SET NAMES utf8mb4;\nDELIMITER //\nCREATE /* comment */ DATABASE `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin//\nDELIMITER ;\nCREATE SCHEMA IF NOT EXISTS `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;\n"
	d, _ := newRestoreDefaults(Config{Database: "app"})
	var offsets []int64
	d.onCreate = func(offset, size int64) error { offsets = append(offsets, offset); return nil }
	if err := scanDumpPackets(t.Context(), strings.NewReader(sql), 1<<20, "", nil, d.inspect); err != nil {
		t.Fatal(err)
	}
	if len(offsets) != 1 {
		t.Fatalf("conditional create insertions: %v", offsets)
	}
	offset := offsets[0]
	got := sql[:offset] + " IF NOT EXISTS " + sql[offset:]
	want := strings.Replace(sql, "DATABASE `app`", "DATABASE IF NOT EXISTS  `app`", 1)
	if got != want {
		t.Fatalf("insertion offset changed SQL:\n%s", got)
	}
}

func TestRestoreDatabaseComments(t *testing.T) {
	for _, tc := range []struct{ name, sql, comment string }{
		{"native", "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'business data'", "business data"},
		{"escapes", `CREATE DATABASE app COMMENT='it\'s \\ data\n日本語; */ --'`, "it's \\ data\n日本語; */ --"},
		{"doubled", `CREATE DATABASE app COMMENT 'it''s data'`, "it's data"},
		{"empty", `CREATE DATABASE app COMMENT ''`, ""},
		{"omitted", `CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin`, ""},
		{"equivalent_empty", `CREATE DATABASE app; CREATE DATABASE app COMMENT ''`, ""},
		{"equivalent_omitted", `CREATE DATABASE app COMMENT ''; CREATE DATABASE app`, ""},
		{"alter_keeps_source_default", `CREATE DATABASE app; ALTER DATABASE app COMMENT 'temporary'`, ""},
		{"no_backslash", `SET SQL_MODE='NO_BACKSLASH_ESCAPES'; CREATE DATABASE app COMMENT 'it''s \n data'`, `it's \n data`},
		{"executable", `CREATE DATABASE app /*!40100 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin */ /*M!100500 COMMENT 'business data' */`, "business data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newRestoreDefaults(Config{Database: "app"})
			if err := scanDumpPackets(t.Context(), strings.NewReader(tc.sql+";"), 1<<20, "", nil, d.inspect); err != nil {
				t.Fatal(err)
			}
			if got, ok := d.comments["app"]; !ok || got != tc.comment {
				t.Fatalf("comment = %q (%t), want %q", got, ok, tc.comment)
			}
			for _, noBackslash := range []bool{false, true} {
				l := databaseSQLLexer{s: quoteDatabaseComment(tc.comment, noBackslash), noBackslash: noBackslash}
				got, err := l.next()
				if err != nil || got != tc.comment || l.s != "" {
					t.Fatalf("comment literal roundtrip: %q, %v", got, err)
				}
			}
		})
	}
	for _, sql := range []string{
		`CREATE DATABASE app COMMENT unquoted;`,
		"CREATE DATABASE app COMMENT `identifier`;",
		`SET SQL_MODE='ANSI_QUOTES'; CREATE DATABASE app COMMENT "identifier";`,
		`CREATE DATABASE app COMMENT 'first'; CREATE DATABASE app COMMENT 'second';`,
		`CREATE DATABASE app COMMENT 'first'; CREATE DATABASE app;`,
		`CREATE DATABASE app; CREATE DATABASE app COMMENT 'second';`,
		`SET NAMES latin1; CREATE DATABASE app COMMENT 'data';`,
	} {
		d, _ := newRestoreDefaults(Config{Database: "app"})
		if err := scanDumpPackets(t.Context(), strings.NewReader(sql), 1<<20, "", nil, d.inspect); err == nil {
			t.Fatalf("accepted unsupported comment: %s", sql)
		}
	}
}

func TestDatabaseMetadataMultibyteIdentifiers(t *testing.T) {
	for _, tc := range []struct{ charset, character string }{
		{"sjis", "\x81\x60"}, {"cp932", "\x81\x60"},
		{"big5", "\xa4\x60"}, {"gbk", "\x81\x60"},
	} {
		t.Run(tc.charset, func(t *testing.T) {
			d, _ := newRestoreDefaults(Config{Database: "app"})
			// The trail backtick must not combine with the closing backtick
			// into an escaped quote and consume the following @ or AS clause.
			sql := "SET NAMES " + tc.charset + ";\n" +
				"CREATE DEFINER=`" + tc.character + "`@`localhost` PROCEDURE p() SELECT 1;\n" +
				"CREATE DEFINER=`" + tc.character + "`@`localhost` VIEW `" + tc.character + "` AS SELECT 1;\n" +
				"SET NAMES utf8mb4; CREATE TABLE `ordinary`(id INT);"
			if err := scanDumpPackets(t.Context(), strings.NewReader(sql), 1<<20, "", nil, d.inspect); err != nil {
				t.Fatal(err)
			}
			if len(d.tables) != 2 || !d.tables[[2]string{"app", tc.character}] || !d.tables[[2]string{"app", "ordinary"}] {
				t.Fatalf("metadata names changed: %v", d.tables)
			}
		})
	}
}

func TestDatabaseMetadataMultibyteQuotedTokens(t *testing.T) {
	for _, charset := range []string{"sjis", "cp932", "big5", "gbk"} {
		lead := "\x81"
		if charset == "big5" {
			lead = "\xa4"
		}
		for _, quote := range []string{"`", "'", "\""} {
			for _, noBackslash := range []bool{false, true} {
				prefix := lead + "\\" + lead + "`"
				value := prefix + quote + "suffix"
				// Double the actual quote, preserving both multibyte characters.
				encoded := prefix + quote + quote + "suffix"
				l := databaseSQLLexer{s: quote + encoded + quote + " @ `host`", charset: dumpCharset{current: charset}, noBackslash: noBackslash}
				got, err := l.next()
				if err != nil || got != value {
					t.Fatalf("%s %s token: got %q, want %q, error %v", charset, quote, got, value, err)
				}
				if next, err := l.next(); err != nil || next != "@" {
					t.Fatalf("token consumed following syntax: %q, %v", next, err)
				}
			}
		}
	}
}

func TestDatabaseMetadataTracksCharsetAfterSandboxHeader(t *testing.T) {
	d, _ := newRestoreDefaults(Config{Database: "app"})
	sql := "/*M!999999\\- enable the sandbox mode */\n-- native header\n" +
		"/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;\n" +
		"SET NAMES latin1; SET character_set_client=@OLD_CHARACTER_SET_CLIENT;\n" +
		"CREATE DATABASE app COMMENT '日本語';"
	if err := scanDumpPackets(t.Context(), strings.NewReader(sql), 1<<20, "", nil, d.inspect); err != nil {
		t.Fatal(err)
	}
	if d.comments["app"] != "日本語" {
		t.Fatal(d.comments)
	}
}
