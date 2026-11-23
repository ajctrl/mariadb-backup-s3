package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// mariadb-dump hashes the bytes of db.table using latin1_swedish_ci for both
// ignore lists, even when table names on the server are case sensitive.
func exclusionConflict(options []string) string {
	var conflicts []string
	for _, option := range options {
		key, name, ok := strings.Cut(option, "=")
		if !ok || (key != "--ignore-table" && key != "--ignore-table-data") {
			continue
		}
		literal := "X'" + hex.EncodeToString([]byte(name)) + "'"
		fullName := "BINARY CONCAT(t.TABLE_SCHEMA, '.', t.TABLE_NAME)"
		conflicts = append(conflicts, "(CONVERT("+fullName+" USING latin1) COLLATE latin1_swedish_ci = _latin1 "+literal+" AND "+fullName+" <> "+literal+")")
	}
	if len(conflicts) == 0 {
		return "FALSE"
	}
	return strings.Join(conflicts, " OR ")
}

// Compute database-wide grants once per database. The binary LIKE matches
// MariaDB's byte-wise schema privilege patterns. Table grants are materialized
// once and joined by binary name to avoid scanning all grants for each table.
func metadataQuery(database, conflict string, includeExternal, includeRoles bool) string {
	db := sqlText(database)
	identifier := func(expr string) string {
		return "BINARY IF(@@lower_case_table_names=0," + expr + ",LOWER(" + expr + "))"
	}
	scope := identifier(db)
	engine := "NULL"
	if !includeExternal {
		engine = "IF(t.ENGINE IN ('MRG_MyISAM','MRG_ISAM','CONNECT','OQGRAPH','SPIDER','VP','FEDERATED','FEDERATEDX'), t.ENGINE, NULL)"
	}
	roleCTE, roleGlobal, roleSchema, roleTables := "", "", "", ""
	if includeRoles {
		roleCTE = `, role_grants AS (
 SELECT j.* FROM JSON_TABLE(@backup_role_grants, '$[*]' COLUMNS (
   role_name TEXT PATH '$.Role', db_name TEXT PATH '$.DB', table_name TEXT PATH '$.Tbl',
   tr BOOL PATH '$.Trigger', rd BOOL PATH '$.Read', routine BOOL PATH '$.ShowRoutine',
   from_public BOOL PATH '$.FromPublic'
 )) j WHERE j.from_public OR BINARY j.role_name IN (SELECT BINARY ROLE_NAME FROM information_schema.ENABLED_ROLES)
)`
		roleGlobal = " UNION ALL SELECT tr, rd, routine FROM role_grants WHERE db_name IS NULL AND table_name IS NULL"
		roleSchema = " UNION ALL SELECT 1+from_public, " + identifier("db_name") + ", tr, rd, routine FROM role_grants WHERE db_name IS NOT NULL AND table_name IS NULL"
		roleTables = " UNION ALL SELECT " + identifier("table_name") + " FROM role_grants WHERE tr AND " + identifier("db_name") + "=" + scope
	}
	return `WITH principals AS (
 SELECT CONCAT(QUOTE(LEFT(CURRENT_USER(), CHAR_LENGTH(CURRENT_USER())-CHAR_LENGTH(SUBSTRING_INDEX(CURRENT_USER(),'@',-1))-1)), '@', QUOTE(SUBSTRING_INDEX(CURRENT_USER(),'@',-1))) AS grantee
)` + roleCTE + `, schema_rows(origin, pattern, tr, rd, routine) AS (
 SELECT 0, ` + identifier("g.TABLE_SCHEMA") + `, g.PRIVILEGE_TYPE='TRIGGER', g.PRIVILEGE_TYPE='SELECT', g.PRIVILEGE_TYPE='SHOW CREATE ROUTINE'
 FROM information_schema.SCHEMA_PRIVILEGES g JOIN principals p ON BINARY p.grantee=g.GRANTEE` + roleSchema + `
), patterns AS (
 SELECT origin, pattern, MAX(tr) tr, MAX(rd) rd, MAX(routine) routine,
   pattern REGEXP X'` + hex.EncodeToString([]byte(`^(?:[^\\%_]|\\.)*$`)) + `' AS is_literal
 FROM schema_rows WHERE ` + scope + ` LIKE pattern GROUP BY origin, pattern
), privileges(tr, rd, routine) AS (
 SELECT g.PRIVILEGE_TYPE='TRIGGER', g.PRIVILEGE_TYPE='SELECT', g.PRIVILEGE_TYPE='SHOW CREATE ROUTINE'
 FROM information_schema.USER_PRIVILEGES g JOIN principals p ON BINARY p.grantee=g.GRANTEE` + roleGlobal + `
 UNION ALL SELECT COALESCE(MIN(IF(is_literal,tr,NULL)),MIN(tr)),
   COALESCE(MIN(IF(is_literal,rd,NULL)),MIN(rd)),
   COALESCE(MIN(IF(is_literal,routine,NULL)),MIN(routine)) FROM patterns GROUP BY origin
), db_access AS (
 SELECT COALESCE(MAX(tr),0) tr, COALESCE(MAX(rd),0) rd, COALESCE(MAX(routine),0) routine FROM privileges
), table_triggers AS (
 SELECT DISTINCT names.tbl FROM (
   SELECT ` + identifier("g.TABLE_NAME") + ` tbl FROM information_schema.TABLE_PRIVILEGES g JOIN principals p ON BINARY p.grantee=g.GRANTEE
   WHERE g.PRIVILEGE_TYPE='TRIGGER' AND ` + identifier("g.TABLE_SCHEMA") + `=` + scope + roleTables + `
 ) names
)
SELECT HEX(` + db + `), '', 'missing SELECT' FROM db_access WHERE NOT rd
UNION ALL SELECT HEX(` + db + `), '', 'missing routine access' FROM db_access WHERE NOT routine AND NOT @backup_proc_read
UNION ALL SELECT HEX(t.TABLE_SCHEMA), HEX(t.TABLE_NAME),
 CASE WHEN ` + conflict + ` THEN 'ambiguous exclusion'
 WHEN t.TABLE_TYPE IN ('BASE TABLE','SYSTEM VERSIONED') AND NOT a.tr AND g.tbl IS NULL THEN 'missing TRIGGER'
 ELSE ` + engine + ` END AS issue
 FROM information_schema.TABLES t CROSS JOIN db_access a LEFT JOIN table_triggers g ON ` + identifier("t.TABLE_NAME") + `=g.tbl
 WHERE t.TABLE_SCHEMA=` + db + ` AND ` + identifier("t.TABLE_SCHEMA") + `=` + scope + `
 HAVING issue IS NOT NULL;
`
}

func sqlText(value string) string {
	return "CONVERT(X'" + hex.EncodeToString([]byte(value)) + "' USING utf8mb4)"
}

type tableIssue struct{ db, table, reason string }

func (a *App) checkTables(ctx context.Context, r commandRunner, databases, options []string) error {
	conflict := exclusionConflict(options)
	var roleSetup string
	rolesChecked := false
	for batch := range slices.Chunk(databases, 32) {
		issues, err := a.checkTableBatch(ctx, r, batch, options, conflict, roleSetup)
		if err != nil {
			return err
		}
		if !rolesChecked && slices.ContainsFunc(issues, func(i tableIssue) bool { return strings.HasPrefix(i.reason, "missing ") }) {
			rolesChecked = true
			roleSetup, err = a.roleGrantSetup(ctx, r)
			if err == nil && roleSetup != "" {
				var verified []tableIssue
				verified, err = a.checkTableBatch(ctx, r, batch, options, conflict, roleSetup)
				if err == nil {
					issues = verified
				}
			}
			if err != nil {
				// Keep the original visibility warnings if optional role inspection
				// fails. An unresolved, required TRIGGER grant still fails closed.
				roleSetup = ""
				if ctx.Err() != nil || slices.ContainsFunc(issues, func(i tableIssue) bool {
					return i.reason == "missing TRIGGER" && !slices.Contains(options, "--ignore-table="+i.db+"."+i.table)
				}) {
					return err
				}
			}
		}
		for _, issue := range issues {
			name := issue.db + "." + issue.table
			switch issue.reason {
			case "missing SELECT":
				fmt.Fprintf(a.Err, "Warning: cannot verify schema-wide SELECT on %q; tables or views may be missing from this backup.\n", issue.db)
			case "missing routine access":
				fmt.Fprintf(a.Err, "Warning: cannot verify access to all routine definitions in %q; procedures or functions may be missing from this backup.\n", issue.db)
			case "ambiguous exclusion":
				return fmt.Errorf("dump exclusion also matches %q; refusing ambiguous backup", name)
			default:
				if slices.Contains(options, "--ignore-table="+name) {
					continue
				}
				if issue.reason == "missing TRIGGER" {
					return fmt.Errorf("cannot verify TRIGGER privilege for %q", name)
				}
				if !slices.Contains(options, "--no-data-med=0") && !slices.Contains(options, "--ignore-table-data="+name) {
					return fmt.Errorf("%s table %q needs a separate backup or explicit exclusion", issue.reason, name)
				}
			}
		}
	}
	return nil
}

func (a *App) checkTableBatch(ctx context.Context, r commandRunner, batch, options []string, conflict, roleSetup string) ([]tableIssue, error) {
	var query strings.Builder
	// Let the server check effective access (including roles) without reading
	// any routine definitions. Other errors still fail the metadata query.
	query.WriteString(`SET @backup_proc_read=1;
DELIMITER //
BEGIN NOT ATOMIC
 DECLARE CONTINUE HANDLER FOR 1044,1142,1143,1146 SET @backup_proc_read=0;
 SELECT * FROM mysql.proc LIMIT 0;
END//
DELIMITER ;
`)
	query.WriteString(roleSetup)
	for _, db := range batch {
		query.WriteString(metadataQuery(db, conflict, slices.Contains(options, "--no-data-med=0"), roleSetup != ""))
	}
	var out bytes.Buffer
	args := append(a.connectionArgs(), "--batch", "--raw", "--skip-column-names", "--skip-print-query-on-error")
	if err := r.runInput(ctx, strings.NewReader(query.String()), &out, "mariadb", args...); err != nil {
		return nil, fmt.Errorf("check table metadata: %w", err)
	}
	var result []tableIssue
	for line := range strings.SplitSeq(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid table metadata")
		}
		db, e1 := hex.DecodeString(fields[0])
		table, e2 := hex.DecodeString(fields[1])
		if e1 != nil || e2 != nil {
			return nil, fmt.Errorf("invalid identifier in table metadata")
		}
		result = append(result, tableIssue{string(db), string(table), fields[2]})
	}
	return result, nil
}
