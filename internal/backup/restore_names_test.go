package backup

import (
	"strings"
	"testing"
)

func TestRestoreNameCollisions(t *testing.T) {
	for _, tc := range []struct{ name, sql, want string }{
		{"databases", "CREATE DATABASE CaseAudit; CREATE DATABASE caseaudit;", "database name collision"},
		{"tables", "USE app; CREATE TABLE T(id INT); CREATE TABLE t(id INT);", "table name collision"},
		{"qualified", "CREATE TABLE app.T(id INT); CREATE TABLE `app`.`t`(id INT);", "table name collision"},
		{"quoted", "USE app; CREATE TABLE `T``x`(id INT); CREATE TABLE `t``x`(id INT);", "table name collision"},
		{"newline_collision", "USE app; CREATE TABLE `T\nx`(id INT); CREATE TABLE `t\nx`(id INT);", "table name collision"},
		{"line_endings_distinct", "USE app; CREATE TABLE `t\nx`(id INT); CREATE TABLE `t\rx`(id INT); CREATE TABLE tx(id INT);", ""},
		{"views", "USE app; CREATE TABLE V(id INT); CREATE ALGORITHM=UNDEFINED DEFINER=`root`@`localhost` SQL SECURITY DEFINER VIEW `v` AS SELECT 1;", "table name collision"},
		{"sequences", "USE app; CREATE SEQUENCE S; CREATE SEQUENCE s;", "table name collision"},
		{"replace", "USE app; CREATE TABLE T(id INT); CREATE OR REPLACE TABLE t(id INT);", "table name collision"},
		{"conditional", "USE app; CREATE TABLE IF NOT EXISTS T(id INT); CREATE TABLE IF NOT EXISTS t(id INT);", "table name collision"},
		{"unicode", "USE app; CREATE TABLE `É`(id INT);", "cannot safely compare non-ASCII"},
		{"distinct", "USE app; CREATE TABLE t(id INT); CREATE TABLE u(id INT);", ""},
		{"different_databases", "USE app; CREATE TABLE t(id INT); USE other; CREATE TABLE T(id INT);", ""},
		{"view_placeholder", "USE app; CREATE TABLE v(id INT); CREATE VIEW v AS SELECT 1;", ""},
		{"routine_body", "USE app; CREATE TABLE t(id INT); DELIMITER ;;\nCREATE PROCEDURE p() BEGIN CREATE TABLE T(id INT); END;;\nDELIMITER ;\n", ""},
	} {
		for _, mode := range []int{0, 1, 2} {
			t.Run(tc.name+string(rune('0'+mode)), func(t *testing.T) {
				d, _ := newRestoreDefaults(Config{BackupMode: "combined"})
				d.lowerCaseTableNames = mode
				sql := strings.ReplaceAll(tc.sql, "; DELIMITER", ";\nDELIMITER")
				err := scanDumpPackets(t.Context(), strings.NewReader(sql), 1<<20, "", nil, d.inspect)
				if err == nil {
					_, err = d.resolveNames()
				}
				want := tc.want
				if mode == 0 {
					want = ""
				}
				if want == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("got %v, want %s", err, want)
				}
			})
		}
	}
}

func TestRestorePlanNameResolution(t *testing.T) {
	const declaration = "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin COMMENT 'saved';"
	const override = `{"ApP":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`
	for _, mode := range []int{0, 1, 2} {
		for _, tc := range []struct {
			name, sql, database, explicit, want string
			combined                            bool
		}{
			{name: "reference_alias", sql: declaration + "USE APP; CREATE TABLE t(id INT);", database: "APP"},
			{name: "reference_before_declaration", sql: "USE APP; CREATE TABLE t(id INT);" + declaration, database: "APP"},
			{name: "qualified_alias_placeholder", sql: declaration + "USE APP; CREATE TABLE app.v(id INT); CREATE VIEW APP.v AS SELECT 1;", database: "APP"},
			{name: "override_alias", sql: declaration + "USE app;", database: "app", explicit: override},
			{name: "combined", sql: declaration + "USE APP;", combined: true},
			{name: "table_collision_through_alias", sql: declaration + "CREATE TABLE app.T(id INT); CREATE TABLE APP.t(id INT);", combined: true, want: "table name collision"},
			{name: "database_declaration_collision", sql: declaration + "CREATE DATABASE APP;", combined: true, want: "database name collision"},
			{name: "conflicting_alias_overrides", sql: declaration, database: "app", explicit: `{"APP":{"character_set":"latin1","collation":"latin1_swedish_ci"},"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`, want: "conflicting aliases"},
			{name: "override_metadata_conflict", sql: declaration, database: "app", explicit: `{"APP":{"character_set":"latin1","collation":"latin1_swedish_ci"}}`, want: "conflicts with dump metadata"},
			{name: "equivalent_alias_overrides", sql: declaration, database: "app", explicit: `{"APP":{"character_set":"utf8mb4","collation":"utf8mb4_bin"},"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`},
			{name: "undeclared_ambiguous", sql: "USE app; CREATE TABLE t(id INT); USE APP; CREATE TABLE t(id INT);", combined: true, explicit: override, want: "without a unique CREATE DATABASE"},
			{name: "unused_override", sql: declaration, database: "app", explicit: `{"other":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}`, want: "unused database"},
			{name: "different_database", sql: declaration, database: "other", want: "differs from MARIADB_DATABASE"},
		} {
			t.Run(tc.name+string(rune('0'+mode)), func(t *testing.T) {
				config := Config{Database: tc.database, RestoreDatabaseDefaults: tc.explicit}
				if tc.combined {
					config.BackupMode = "combined"
				}
				d, err := newRestoreDefaults(config)
				if err != nil {
					t.Fatal(err)
				}
				d.lowerCaseTableNames = mode
				if err := scanDumpPackets(t.Context(), strings.NewReader(tc.sql), 1<<20, "", nil, d.inspect); err != nil {
					t.Fatal(err)
				}
				p, err := d.plan()
				// Every case in this table uses at least one different spelling
				// or genuinely different DB; case-sensitive destinations must
				// continue to reject them rather than silently resolve an alias.
				if mode == 0 {
					if err == nil {
						t.Fatalf("case-sensitive destination accepted alias: %+v", p)
					}
					return
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
				if len(p.databases) != 1 {
					t.Fatalf("plan must contain one DB: %+v", p)
				}
				db := p.databases[0]
				if db.name != "app" || db.defaults != (databaseDefaults{"utf8mb4", "utf8mb4_bin"}) || db.comment == nil || *db.comment != "saved" {
					t.Fatalf("metadata was not attached to resolved DB: %+v", db)
				}
				if !tc.combined && p.selected != "app" {
					t.Fatalf("selected DB not resolved: %s", p.selected)
				}
			})
		}
	}
}
