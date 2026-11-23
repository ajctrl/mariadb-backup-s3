package backup

import (
	"os"
	"strings"
	"testing"
)

func TestPacketPreflightUsesStatements(t *testing.T) {
	const routine = "CREATE PROCEDURE p() BEGIN\n SELECT 'a;''b', `c;d`; /* ; */ SELECT 2; END"
	for _, tc := range []struct {
		input   string
		largest int
	}{
		{"SET @a='a;\\'b';SELECT 2;", len("SET @a='a;\\'b'")},
		{"DELIMITER ;;\n" + routine + ";;\nDELIMITER ;\nSELECT 1;", len(routine) - len("/* ; */") + 1},
		{"SELECT 1 -- ;\n+ 2;# ;\nSELECT 2;", len("SELECT 1 \n+ 2")},
		{"INSERT " + strings.Repeat("0", 150000), 150007},
	} {
		size, err := largestStatement(t.Context(), strings.NewReader(tc.input), "")
		if err != nil || size != int64(tc.largest) {
			t.Errorf("size=%d want=%d err=%v", size, tc.largest, err)
		}
	}
	// Many short INSERTs can exceed the limit in aggregate and still fit on the wire.
	f, err := os.CreateTemp(t.TempDir(), "dump")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.WriteString(strings.Repeat("SELECT 1;\n", 500)); err != nil {
		t.Fatal(err)
	}
	if _, err = f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := checkPacketSize(t.Context(), f, 128, ""); err != nil {
		t.Fatal(err)
	}
	pos, err := f.Seek(0, 1)
	if err != nil || pos != 0 {
		t.Fatalf("import file offset=%d err=%v", pos, err)
	}
}

func TestRestoreDiagnosticRedactsServerMessage(t *testing.T) {
	var d restoreDiagnostic
	for _, part := range []string{"unrelated warning\nER", "ROR 1062 (23000) at line 8", ": Duplicate entry 'private value'\nsecond private line\n"} {
		d.Write([]byte(part))
	}
	if err := d.err(3); err == nil || err.Error() != "ERROR 1062 (23000) at line 5" {
		t.Fatalf("diagnostic=%v", err)
	}
}

func TestPacketPreflightTracksDumpSQLMode(t *testing.T) {
	save := "/*!50003 SET @saved_sql_mode = @@sql_mode */;"
	switchMode := "/*!50003 SET sql_mode = 'NO_BACKSLASH_ESCAPES' */;"
	restore := "/*!50003 SET sql_mode = @saved_sql_mode */;"
	// A trailing backslash is literal in this mode. Under the default mode a
	// backslash escapes the quote, so ';' inside the following value is data.
	literal := "INSERT INTO t VALUES ('" + strings.Repeat("x", 200) + "\\');"
	escaped := "INSERT INTO t VALUES ('a\\';" + strings.Repeat("y", 300) + "');"
	input := strings.Join([]string{save, switchMode, literal, restore, escaped}, "\n")
	got, err := largestStatement(t.Context(), strings.NewReader(input), "")
	if err != nil || got != int64(len(escaped)-1) {
		t.Fatalf("mode-aware size=%d err=%v", got, err)
	}
}
