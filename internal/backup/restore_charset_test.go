package backup

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDumpPacketsWithMixedCharsets(t *testing.T) {
	for charset, character := range map[string]string{
		"sjis": "\x95\x5c", "cp932": "\x95\x5c", "big5": "\xa4\x5c", "gbk": "\x81\x5c",
	} {
		t.Run(charset, func(t *testing.T) {
			header := "/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;\n" +
				"/*!50003 SET character_set_client=" + charset + " */;\nDELIMITER ;;\n"
			// Include a trail backslash before a closing quote, an actual
			// escaped backslash, and a quoted delimiter. Restoring the saved
			// charset must also restore UTF8 lexical rules for later SQL.
			body := "CREATE PROCEDURE p() SELECT '" + character + "', '\\\\" + character + ";;';;\nDELIMITER ;\n"
			footer := "/*!50003 SET character_set_client=@OLD_CHARACTER_SET_CLIENT */;\nSELECT '日本語\\\\';"
			if err := checkDumpPackets(t.Context(), strings.NewReader(header+body+footer), 1024, ""); err != nil {
				t.Fatal(err)
			}
			large := "CREATE PROCEDURE p() SELECT '" + character + ";;" + strings.Repeat("x", 2000) + "';;\n"
			if err := checkDumpPackets(t.Context(), strings.NewReader(header+large), 1024, ""); err == nil || !strings.Contains(err.Error(), "max_allowed_packet") {
				t.Fatalf("oversize multibyte routine accepted: %v", err)
			}
		})
	}
	// 0x8160 contains a backtick in SJIS; it is not the end of an identifier.
	sql := "SET NAMES sjis; SELECT `\x81\x60`;"
	if err := checkDumpPackets(t.Context(), strings.NewReader(sql), 1024, ""); err != nil {
		t.Fatal(err)
	}
	// MariaDB's euckr includes CP949 pairs with ASCII letter trail bytes.
	sql = "SET NAMES euckr;\nDELIMITER X\nSELECT '\x81X'X\nDELIMITER ;\n"
	if err := checkDumpPackets(t.Context(), strings.NewReader(sql), 1024, ""); err != nil {
		t.Fatal(err)
	}
}

func TestCharsetSwitchesPreserveDumpBytesAndOffsets(t *testing.T) {
	// Repeated directives and a multibyte character straddling the reader
	// buffer boundary must not displace any insertion into the SQL body.
	for offset := 65530; offset < 65540; offset++ {
		sql := "SET @saved_cs_client=@@character_set_client;\nDELIMITER ;;\nSET character_set_client='sjis';;\n"
		sql += "CREATE PROCEDURE p() SELECT '" + strings.Repeat("x", offset-len(sql)-len("CREATE PROCEDURE p() SELECT '")) + "\x95\\';;\n"
		sql += "SET character_set_client=@saved_cs_client;;\nDELIMITER ;\nSELECT '日本語';\n"
		var insertions []charsetInsertion
		err := scanDumpPackets(t.Context(), strings.NewReader(sql), 1<<20, "", func(offset int64, cs, delimiter string) error {
			command, err := charsetCommand(cs, delimiter, 1<<20)
			insertions = append(insertions, charsetInsertion{offset, command})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(insertions) != 2 {
			t.Fatalf("got %d charset switches", len(insertions))
		}
		var expected strings.Builder
		last := int64(0)
		for _, insertion := range insertions {
			if !strings.HasSuffix(sql[:insertion.Offset], ";;") || !strings.Contains(insertion.Text, "DELIMITER ;;") {
				t.Fatal("switch was not placed after a complete statement with its delimiter restored")
			}
			expected.WriteString(sql[last:insertion.Offset])
			expected.WriteString(insertion.Text)
			last = insertion.Offset
		}
		expected.WriteString(sql[last:])
		r := testRestoreInput(t, sql)
		encoder := json.NewEncoder(r.plan)
		for _, insertion := range insertions {
			if err := encoder.Encode(insertion); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.rewind(); err != nil {
			t.Fatal(err)
		}
		// Tiny output reads also split multibyte characters and inserted commands.
		var actual bytes.Buffer
		if _, err := io.CopyBuffer(writerOnly{&actual}, readerOnly{r}, make([]byte, 7)); err != nil {
			t.Fatal(err)
		}
		if actual.String() != expected.String() {
			t.Fatal("charset insertion changed the original SQL bytes")
		}
	}
}

type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }

func TestCharsetAssignmentsFailClosed(t *testing.T) {
	for _, sql := range []string{
		"SET character_set_client=@unknown;",
		"SET character_set_client=CONCAT('s','jis');",
		"SET NAMES unknown;", "SET NAMES utf16;",
		"SET CHARACTER SET sjis;", "SET character_set_client=sjis, sql_mode='';",
		"SET NAMES sjis", // Cannot insert a command into unfinished SQL.
		"DELIMITER 日本語\nSELECT 1日本語",
	} {
		err := scanDumpPackets(t.Context(), strings.NewReader(sql), 1024, "", func(int64, string, string) error { return nil })
		if err == nil {
			t.Fatalf("unhandled assignment/directive accepted: %q", sql)
		}
	}
	for _, sql := range []string{
		"SET NAMES 'sjis'; SET NAMES utf8mb4 COLLATE utf8mb4_bin;",
		"SET @a=@@CHARACTER_SET_CLIENT; SET NAMES cp932; SET @b=@@CHARACTER_SET_CLIENT; SET NAMES latin1; SET character_set_client=@b; SELECT '\x95\\'; SET character_set_client=@a;",
	} {
		if err := checkDumpPackets(t.Context(), strings.NewReader(sql), 1024, ""); err != nil {
			t.Fatal(err)
		}
	}
	want := errors.New("plan write failed")
	err := scanDumpPackets(t.Context(), strings.NewReader("SET NAMES sjis;"), 1024, "", func(int64, string, string) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("lost plan write failure: %v", err)
	}
}

func TestRestoreInputWithoutChangesAndTruncatedSource(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		r := testRestoreInput(t, "SELECT 1;")
		if truncated {
			if err := json.NewEncoder(r.plan).Encode(charsetInsertion{100, "must not be inserted"}); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.rewind(); err != nil {
			t.Fatal(err)
		}
		actual, err := io.ReadAll(r)
		if truncated {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncated SQL accepted: %v", err)
			}
		} else if err != nil || string(actual) != "SELECT 1;" {
			t.Fatalf("unmodified SQL: %q, %v", actual, err)
		}
	}
}

func testRestoreInput(t *testing.T, sql string) *restoreInput {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dump.sql")
	if err := os.WriteFile(path, []byte(sql), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	r, err := newRestoreInput(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
		if _, err := os.Stat(r.plan.Name()); !os.IsNotExist(err) {
			t.Error("charset plan was not removed")
		}
	})
	if info, err := r.plan.Stat(); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("charset plan must be private")
	}
	return r
}
