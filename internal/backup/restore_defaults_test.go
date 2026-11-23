package backup

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestPerDatabaseRestoreDefaults(t *testing.T) {
	existing := &databaseDefaults{"utf8mb4", "utf8mb4_bin"}
	const explicit = `{"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`
	const native = "CREATE DATABASE /*!32312 IF NOT EXISTS*/ `app` /*!40100 DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin */;\nUSE `app`;\n"
	const legacy = "-- earlier per-database dump\n"
	const data = "CREATE TABLE t(id INT);\nINSERT INTO t VALUES(1);\n"
	for _, tc := range []struct {
		name, metadata, settings string
		exists, create, warn     bool
		wantErr                  string
	}{
		{name: "create missing", metadata: native, create: true},
		{name: "matching existing", metadata: native, exists: true},
		{name: "different collation", metadata: strings.Replace(native, "utf8mb4_bin", "utf8mb4_general_ci", 1), exists: true, wantErr: "source character_set=\"utf8mb4\", collation=\"utf8mb4_general_ci\"; destination character_set=\"utf8mb4\", collation=\"utf8mb4_bin\""},
		{name: "old dump existing", metadata: legacy, exists: true, warn: true},
		{name: "old dump missing", metadata: legacy, wantErr: "set RESTORE_DATABASE_DEFAULTS"},
		{name: "explicit old dump missing", metadata: legacy, settings: explicit, create: true},
		{name: "explicit mismatch existing", metadata: legacy, exists: true, settings: strings.Replace(explicit, "utf8mb4_bin", "utf8mb4_general_ci", 1), wantErr: "database defaults differ"},
		{name: "partial existing", metadata: "CREATE DATABASE app CHARACTER SET utf8mb4;\n", exists: true, warn: true},
		{name: "known part conflicts", metadata: "CREATE DATABASE app CHARACTER SET latin1;\n", exists: true, wantErr: "database defaults differ"},
		{name: "explicit conflicts", metadata: native, settings: strings.Replace(explicit, "utf8mb4_bin", "utf8mb4_general_ci", 1), wantErr: "conflicts"},
		{name: "override for another DB", metadata: legacy, settings: strings.Replace(explicit, "app", "other", 1), wantErr: "requires character_set"},
		{name: "wrong USE", metadata: "USE other;\n", exists: true, wantErr: "selects database"},
		{name: "wrong CREATE", metadata: strings.Replace(native, "`app`", "`other`", 1), exists: true, wantErr: "differs"},
		{name: "DROP", metadata: "DROP DATABASE app;\n", exists: true, wantErr: "DROP DATABASE"},
		{name: "routine changes existing defaults", metadata: native + "/*!50003 ALTER DATABASE `app` CHARACTER SET latin1 COLLATE latin1_swedish_ci */;\n", exists: true, wantErr: "ALTER DATABASE"},
		{name: "irrelevant ALTER discarded", metadata: native + "ALTER DATABASE app COLLATE utf8mb4_bin;\n", exists: true},
		{name: "comment stays on existing DB", metadata: strings.Replace(native, " */;", " COMMENT 'original' */;", 1), exists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := os.CreateTemp(t.TempDir(), "dump")
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err = f.WriteString(tc.metadata + data); err != nil {
				t.Fatal(err)
			}
			if _, err = f.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			var dest *databaseDefaults
			if tc.exists {
				dest = existing
			}
			var log bytes.Buffer
			a := &App{Config: Config{Database: "app", RestoreDatabaseDefaults: tc.settings}, Err: &log}
			prefix, err := a.prepareDatabase(t.Context(), f, dest, false, 1<<20, "", nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "USE `app`;\n"
			if tc.create {
				want = "CREATE DATABASE `app` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;\n" + want
			}
			if prefix != want || strings.Contains(log.String(), "could not be fully verified") != tc.warn {
				t.Fatalf("prefix=%q warning=%q", prefix, log.String())
			}
			body, err := io.ReadAll(f)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(string(body), data) || strings.Contains(string(body), "CREATE DATABASE") || strings.Contains(string(body), "ALTER DATABASE") {
				t.Fatalf("import SQL: %s", body)
			}
		})
	}
}

func TestRestorePreservesDumpPreambleAndEscapedNames(t *testing.T) {
	const name = "my`db"
	const sandbox = "/*M!999999\\- enable the sandbox mode */"
	sql := sandbox + "\nCREATE DATABASE `my``db` DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'testing';\nDELIMITER //\nCREATE PROCEDURE p() BEGIN SELECT 'ALTER DATABASE literal'; END//\nDELIMITER ;\n"
	f, err := os.CreateTemp(t.TempDir(), "dump")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteString(sql); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	a := &App{Config: Config{Database: name}, Err: io.Discard}
	prefix, err := a.prepareDatabase(t.Context(), f, nil, false, 1<<20, "", nil)
	if err != nil || !strings.Contains(prefix, "CREATE DATABASE `my``db` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'testing';") {
		t.Fatalf("prefix=%q err=%v", prefix, err)
	}
	b, err := io.ReadAll(f)
	if err != nil || !strings.HasPrefix(string(b), sandbox+"\nDO 0") || !strings.Contains(string(b), "SELECT 'ALTER DATABASE literal'") {
		t.Fatalf("SQL %q, err %v", b, err)
	}
}

func TestRestoreMatchesServerIdentifierCase(t *testing.T) {
	calls := 0
	d, err := newRestoreDefaults(Config{Database: "\u00c4pp"}, true, nil, nil, func(sql string) (string, error) {
		calls++
		if !strings.Contains(sql, sqlText("\u00e4pp")) || !strings.Contains(sql, sqlText("\u00c4pp")) {
			t.Fatal(sql)
		}
		return "1", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if ok, err := d.matches("\u00e4pp"); err != nil || !ok {
			t.Fatalf("match=%t err=%v", ok, err)
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
