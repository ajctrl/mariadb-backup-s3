package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"strings"
)

// Transaction IDs in versioning periods cannot be restored as timestamp history.
// Check every selected database before publishing any dump. mariadb-dump still
// rejects unsupported tables introduced by DDL after this preflight.
func (a *App) checkHistorySupport(ctx context.Context, runner commandRunner, databases []string) error {
	// information_schema uses a case-insensitive collation even on servers
	// where differently cased database names identify separate databases.
	schemaName := func(expr string) string {
		return "BINARY IF(@@lower_case_table_names = 0, " + expr + ", LOWER(" + expr + "))"
	}
	opts, err := a.Config.dumpOptions()
	if err != nil {
		return err
	}
	var ignored []string
	for _, opt := range opts {
		if value, ok := strings.CutPrefix(opt, "--ignore-table="); ok {
			ignored = append(ignored, "CONVERT(0x"+hex.EncodeToString([]byte(value))+" USING latin1)")
		}
	}
	queries := make([]string, len(databases))
	for i, database := range databases {
		// A direct equality with a string literal lets information_schema open
		// only this database. IN lists, derived-table joins and CONVERT calls
		// on the RHS all cause MariaDB 11.4 to scan every database instead.
		// A charset-prefixed hex literal is safe under any session SQL mode.
		selected := "_utf8mb4 0x" + hex.EncodeToString([]byte(database))
		query := "SELECT DISTINCT HEX(TABLE_SCHEMA) AS schema_hex, HEX(TABLE_NAME) AS table_hex FROM information_schema.COLUMNS " +
			"WHERE TABLE_SCHEMA = " + selected + " AND " +
			schemaName("TABLE_SCHEMA") + " = " + schemaName(selected) + " " +
			"AND DATA_TYPE = 'bigint' AND GENERATION_EXPRESSION IN ('ROW START', 'ROW END')"
		if len(ignored) > 0 {
			// MariaDB 11.4 mariadb-dump initializes its ignore_table hash with
			// my_charset_latin1 (latin1_swedish_ci), before parsing charset options.
			// It compares raw name bytes, not Unicode case folds. Let the server
			// apply that same collation to those bytes instead of approximating it
			// with Go string equality or strings.EqualFold.
			// The dump uses the supplied database spelling in its hash key, even
			// when the server resolves it to a differently cased database name.
			query += " AND CONVERT(BINARY CONCAT(" + selected + ", '.', TABLE_NAME) USING latin1) " +
				"COLLATE latin1_swedish_ci NOT IN (" + strings.Join(ignored, ",") + ")"
		}
		queries[i] = query
	}
	query := strings.Join(queries, " UNION ") + " ORDER BY schema_hex, table_hex;"
	args := append(a.connectionArgs(), "--batch", "--raw", "--skip-column-names")
	var output bytes.Buffer
	if err := runner.runInput(ctx, strings.NewReader(query), &output, "mariadb", args...); err != nil {
		return fmt.Errorf("could not check system-versioned table support: %w", err)
	}
	var unsupported []string
	for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
		if line == "" && output.Len() == 0 {
			break
		}
		schemaHex, tableHex, ok := strings.Cut(line, "\t")
		schema, schemaErr := hex.DecodeString(schemaHex)
		table, tableErr := hex.DecodeString(tableHex)
		if !ok || schemaErr != nil || tableErr != nil || len(schema) == 0 || len(table) == 0 {
			return fmt.Errorf("invalid system-versioned table metadata result")
		}
		unsupported = append(unsupported, fmt.Sprintf("%q.%q", schema, table))
	}
	if len(unsupported) > 0 {
		return fmt.Errorf("unsupported transaction-precise system-versioned tables: %s; --dump-history supports timestamp-based history only; no dumps were started", strings.Join(unsupported, ", "))
	}
	return nil
}
