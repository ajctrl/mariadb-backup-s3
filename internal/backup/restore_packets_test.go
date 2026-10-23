package backup

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDumpPacketBoundaries(t *testing.T) {
	large := strings.Repeat("x", 700)
	for _, tc := range []struct {
		name, sql, mode, wantError string
	}{
		{"many_small_statements", strings.Repeat("INSERT INTO t VALUES ('"+large+"');\n", 100), "", ""},
		{"one_large_statement", "INSERT INTO t VALUES ('" + large + large + "');", "", "max_allowed_packet=1024"},
		{"quoted_semicolon", "INSERT INTO t VALUES ('" + large + ";\n" + large + "');", "", "max_allowed_packet=1024"},
		{"double_quotes", `INSERT INTO t VALUES ("` + large + `;` + large + `");`, "", "max_allowed_packet=1024"},
		{"quoted_identifier", "SELECT `" + large + ";" + large + "`;", "", "max_allowed_packet=1024"},
		{"identifier_backslash", "SELECT `tail\\`; SELECT 1;", "", ""},
		{"ansi_identifier_backslash", `SELECT "tail\"; SELECT 1;`, "ANSI_QUOTES", ""},
		{"escaped_quote", `INSERT INTO t VALUES ('a\';` + large + large + `');`, "", "max_allowed_packet=1024"},
		{"doubled_quote", `INSERT INTO t VALUES ('a'';` + large + large + `');`, "", "max_allowed_packet=1024"},
		{"comment_semicolon", "SELECT '" + large + "' /* ;\n */ , '" + large + "';", "", "max_allowed_packet=1024"},
		{"line_comment", "SELECT '" + large + "' -- ;\n , '" + large + "';", "", "max_allowed_packet=1024"},
		{"hash_comment", "SELECT '" + large + "' # ;\n , '" + large + "';", "", "max_allowed_packet=1024"},
		{"routine", "DELIMITER ;;\nCREATE PROCEDURE p() BEGIN SELECT '" + large + "'; SELECT '" + large + "'; END;;\nDELIMITER ;\n", "", "max_allowed_packet=1024"},
		{"routine_and_normal_sql", "DELIMITER $$\nCREATE PROCEDURE p() BEGIN SELECT '$$'; END$$\nDELIMITER ;\n" + strings.Repeat("SELECT '"+large+"';\n", 10), "", ""},
		{"executable_comment_quoted_end", "DELIMITER ;;\n/*!50003 CREATE PROCEDURE p() BEGIN SELECT '" + large + "*/;;" + large + "'; END */;;\nDELIMITER ;\n", "", "max_allowed_packet=1024"},
		{"mode_save_set_restore", "/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;\n" + `SELECT 'it\'s';` + "\n/*!50003 SET @saved_sql_mode = @@sql_mode */;\n/*!50003 SET sql_mode = 'NO_BACKSLASH_ESCAPES' */;\nSELECT 'a\\';\n/*!50003 SET sql_mode = @saved_sql_mode */;\n" + `SELECT 'it\'s';` + "\n/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;\nSELECT 'a\\';", "NO_BACKSLASH_ESCAPES", ""},
		{"unknown_mode", "SET SQL_MODE=CONCAT(@@SQL_MODE,',NO_BACKSLASH_ESCAPES');", "", "unsupported SQL_MODE"},
		{"unknown_saved_mode", "SET SQL_MODE=@not_saved;", "", "unknown saved SQL_MODE"},
		{"mariadb_mode_comment", "/*M!100100 SET SQL_MODE='NO_BACKSLASH_ESCAPES' */; SELECT 'tail\\';", "", ""},
		{"unterminated_quote", "SELECT 'secret", "", "unterminated"},
		{"unterminated_comment", "SELECT 1 /* secret", "", "unterminated"},
		{"delimiter_with_pending_sql", "SELECT 1\nDELIMITER ;;\n", "", "invalid DELIMITER"},
		{"sandbox_comment", "/*M!999999\\- enable the sandbox mode */\n-- header\nSELECT 1;\n", "", ""},
		{"final_statement_without_delimiter", "SELECT '" + large + large + "'", "", "max_allowed_packet=1024"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDumpPackets(t.Context(), strings.NewReader(tc.sql), 1024, tc.mode)
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantError)
			}
		})
	}
}

func TestDumpPacketReadAndCancellationFailures(t *testing.T) {
	want := errors.New("read failure")
	err := checkDumpPackets(t.Context(), io.MultiReader(strings.NewReader("SELECT 1;\n"), failingDumpReader{want}), 1024, "")
	if !errors.Is(err, want) {
		t.Fatalf("read error = %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := checkDumpPackets(ctx, strings.NewReader("SELECT 1;"), 1024, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestDumpPacketAcrossReaderBuffer(t *testing.T) {
	// Quoted delimiters and escaped quotes must remain quoted across a refill.
	sql := "INSERT INTO t VALUES ('" + strings.Repeat("x", 65512) + `\';` + strings.Repeat("x", 70000) + `');`
	if err := checkDumpPackets(t.Context(), strings.NewReader(sql), 131072, ""); err == nil || !strings.Contains(err.Error(), "max_allowed_packet") {
		t.Fatalf("large split statement accepted: %v", err)
	}
	// The line-start directive peek can trigger a refill. Try each position
	// around the buffer edge so it cannot change the quote/delimiter state.
	for offset := 65530; offset < 65540; offset++ {
		padding := strings.Repeat("SELECT 0;\n", 6552) + strings.Repeat(" ", offset-65520-1) + "\n"
		query := "SELECT '" + strings.Repeat("x", 70000) + ";" + strings.Repeat("x", 70000) + "';"
		if err := checkDumpPackets(t.Context(), strings.NewReader(padding+query), 131072, ""); err == nil || !strings.Contains(err.Error(), "max_allowed_packet") {
			t.Fatalf("statement starting at offset %d: %v", offset, err)
		}
	}
	// Long executable routine comments need not fit in the mode-inspection
	// prefix buffer, and their internal semicolons cannot split packets.
	routine := "DELIMITER ;;\n/*!50003 CREATE PROCEDURE p() BEGIN SELECT '" + strings.Repeat("x", 70000) + "'; END */;;\nDELIMITER ;\n"
	if err := checkDumpPackets(t.Context(), strings.NewReader(routine), 131072, ""); err != nil {
		t.Fatalf("large routine: %v", err)
	}
}

func TestDumpPacketReservesProtocolBytes(t *testing.T) {
	for _, limit := range []int64{9, 10} {
		err := checkDumpPackets(t.Context(), strings.NewReader("SELECT 1;"), limit, "")
		if (err == nil) != (limit == 10) {
			t.Fatalf("limit %d: %v", limit, err)
		}
	}
}

type failingDumpReader struct{ err error }

func (r failingDumpReader) Read([]byte) (int, error) { return 0, r.err }
