package backup

import (
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

func TestCharsetAssignmentsFailClosed(t *testing.T) {
	for _, sql := range []string{
		"SET character_set_client=@unknown;",
		"SET character_set_client=CONCAT('s','jis');",
		"SET NAMES unknown;", "SET NAMES utf16;",
		"SET CHARACTER SET sjis;", "SET character_set_client=sjis, sql_mode='';",
		"DELIMITER 日本語\nSELECT 1日本語",
	} {
		err := checkDumpPackets(t.Context(), strings.NewReader(sql), 1024, "")
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
}
