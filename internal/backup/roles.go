package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

// Keep identifiers intact while locating ON and TO. A quoted column name can
// contain commas, privilege names, backticks, or even an entire GRANT clause.
const quotedIdentifier = "`(?:[^`]|``)*`"

var grantScope = regexp.MustCompile("(?s)^GRANT ((?:" + quotedIdentifier + "|[^`])+?) ON (\\*|" + quotedIdentifier + ")\\.(\\*|" + quotedIdentifier + ") TO (PUBLIC|" + quotedIdentifier + ")(?: WITH GRANT OPTION)?$")
var identifierText = regexp.MustCompile(quotedIdentifier)

type roleGrant struct {
	Role                       string
	DB, Tbl                    *string // nil denotes an unquoted wildcard
	Trigger, Read, ShowRoutine bool
	FromPublic                 bool
}

func grantIdentifier(value string) *string {
	if value == "*" {
		return nil
	}
	name := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(value, "`"), "`"), "``", "`")
	return &name
}

func parseRoleGrant(statement string) (roleGrant, bool) {
	parts := grantScope.FindStringSubmatch(statement)
	if parts == nil {
		return roleGrant{}, false
	}
	privileges := strings.Split(identifierText.ReplaceAllString(parts[1], "?"), ", ")
	all := slices.Contains(privileges, "ALL PRIVILEGES")
	g := roleGrant{
		Role: *grantIdentifier(parts[4]), DB: grantIdentifier(parts[2]), Tbl: grantIdentifier(parts[3]),
		Trigger:     slices.Contains(privileges, "TRIGGER") || all,
		Read:        slices.Contains(privileges, "SELECT") || all,
		ShowRoutine: slices.Contains(privileges, "SHOW CREATE ROUTINE") || all,
	}
	// Include schema patterns even when they only grant unrelated privileges:
	// a specific pattern can shadow a wildcard for the same principal.
	return g, g.Trigger || (g.Tbl == nil && (g.DB != nil || g.Read || g.ShowRoutine))
}

func (a *App) roleGrantSetup(ctx context.Context, runner commandRunner) (string, error) {
	// SHOW GRANTS FOR CURRENT_ROLE includes transitively granted roles.
	// The two SHOW statements only inspect roles, never account password hashes.
	const query = "SET SESSION sql_mode=''; SET SESSION sql_quote_show_create=1; " +
		"EXECUTE IMMEDIATE IF(CURRENT_ROLE() IS NULL,'DO 0','SHOW GRANTS FOR CURRENT_ROLE'); SHOW GRANTS FOR PUBLIC;"
	args := append(a.connectionArgs(), "--xml", "--skip-print-query-on-error", "--execute="+query)
	var output bytes.Buffer
	if err := runner.run(ctx, &output, "mariadb", args...); err != nil {
		return "", fmt.Errorf("read effective role grants: %w", err)
	}
	var grants []roleGrant
	decoder := xml.NewDecoder(&output)
	for {
		var result struct {
			Statement string `xml:"statement,attr"`
			Rows      []struct {
				Grant string `xml:"field"`
			} `xml:"row"`
		}
		err := decoder.Decode(&result)
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("decode MariaDB role grants: %w", err)
		}
		for _, row := range result.Rows {
			if g, ok := parseRoleGrant(row.Grant); ok {
				g.FromPublic = strings.TrimSpace(result.Statement) == "SHOW GRANTS FOR PUBLIC"
				grants = append(grants, g)
			}
		}
	}
	if len(grants) == 0 {
		return "", nil
	}
	payload, err := json.Marshal(grants)
	if err != nil {
		return "", err
	}
	return "SET @backup_role_grants=CONVERT(X'" + hex.EncodeToString(payload) + "' USING utf8mb4);\n", nil
}
