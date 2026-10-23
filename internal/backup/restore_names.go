package backup

import (
	"fmt"
	"sort"
	"strings"
)

// Inspect only the header of CREATE statements, never routine bodies. Views and
// sequences share the table namespace; native dumps may declare a view twice
// (first as a placeholder table), which is safe when the spelling is identical.
func (d *restoreDefaults) inspectTable(token string, l databaseSQLLexer) (bool, error) {
	for {
		switch strings.ToUpper(token) {
		case "":
			return true, fmt.Errorf("incomplete CREATE metadata header")
		case "TABLE", "VIEW", "SEQUENCE":
			name, err := l.next()
			if err != nil {
				return true, err
			}
			if strings.EqualFold(name, "IF") && !l.quoted {
				not, e1 := l.next()
				exists, e2 := l.next()
				if e1 != nil || e2 != nil || !strings.EqualFold(not, "NOT") || !strings.EqualFold(exists, "EXISTS") {
					return true, fmt.Errorf("unsupported table metadata syntax")
				}
				name, err = l.next()
				if err != nil {
					return true, err
				}
			}
			database := d.currentDatabase
			next, err := l.next()
			if err != nil {
				return true, err
			}
			if next == "." && !l.quoted {
				database = name
				name, err = l.next()
				if err != nil {
					return true, err
				}
			}
			// Quoted table identifiers may contain CR/LF. Keep their exact bytes
			// for collision checks, just as the dump and restore clients do.
			if database == "" || name == "" || strings.ContainsRune(name, '\x00') {
				return true, fmt.Errorf("cannot determine database/table name during restore preflight")
			}
			d.used[database] = true
			d.tables[[2]string{database, name}] = true
			return true, nil
		case "OR":
			next, err := l.next()
			if err != nil || !strings.EqualFold(next, "REPLACE") {
				return true, fmt.Errorf("unsupported CREATE metadata syntax")
			}
		case "TEMPORARY":
		case "ALGORITHM":
			eq, err := l.next()
			if err != nil || eq != "=" {
				return true, fmt.Errorf("unsupported view ALGORITHM syntax")
			}
			if _, err := l.next(); err != nil {
				return true, err
			}
		case "DEFINER":
			// Native dumps use DEFINER=`user`@`host`.
			eq, err := l.next()
			if err != nil || eq != "=" {
				return true, fmt.Errorf("unsupported DEFINER syntax")
			}
			if _, err := l.next(); err != nil {
				return true, err
			}
			at, err := l.next()
			if err != nil || at != "@" {
				return true, fmt.Errorf("unsupported DEFINER syntax")
			}
			if _, err := l.next(); err != nil {
				return true, err
			}
		case "SQL":
			security, err := l.next()
			if err != nil || !strings.EqualFold(security, "SECURITY") {
				return true, fmt.Errorf("unsupported view security syntax")
			}
			if _, err := l.next(); err != nil {
				return true, err
			}
		default:
			return false, nil
		}
		var err error
		token, err = l.next()
		if err != nil {
			return true, err
		}
	}
}

// restoreNames separates object declarations from references. Only an explicit,
// unique CREATE DATABASE declaration can establish that differently spelled
// database references denote the same source object. Without one, retain the
// conservative collision check rather than guessing about source identity.
type restoreNames struct {
	aliases map[string]string
	names   []string
	byKey   map[string]string
	mode    int
}

func (n *restoreNames) key(name string) (string, error) {
	if n.mode == 0 {
		return name, nil
	}
	// MariaDB's identifier mapping is not Go's Unicode case folding.
	for _, b := range []byte(name) {
		if b >= 128 {
			return "", fmt.Errorf("cannot safely compare non-ASCII identifier %q with destination lower_case_table_names=%d", name, n.mode)
		}
	}
	return strings.ToLower(name), nil
}

func (d *restoreDefaults) resolveNames() (*restoreNames, error) {
	n := &restoreNames{aliases: map[string]string{}, byKey: map[string]string{}, mode: d.lowerCaseTableNames}
	if n.mode != 0 && n.mode != 1 && n.mode != 2 {
		return nil, fmt.Errorf("unsupported destination lower_case_table_names=%d", n.mode)
	}
	declared := make([]string, 0, len(d.declared))
	for name := range d.declared {
		declared = append(declared, name)
	}
	sort.Strings(declared)
	for _, name := range declared {
		key, err := n.key(name)
		if err != nil {
			return nil, err
		}
		if old, ok := n.byKey[key]; ok && old != name {
			return nil, fmt.Errorf("database name collision: %q and %q with destination lower_case_table_names=%d", old, name, n.mode)
		}
		n.byKey[key] = name
	}
	references := make([]string, 0, len(d.used)+1)
	for name := range d.used {
		references = append(references, name)
	}
	if d.config.BackupMode != "combined" && !d.used[d.config.Database] {
		references = append(references, d.config.Database)
	}
	if len(references) == 0 {
		return nil, fmt.Errorf("dump contains no database selection; combined restore requires CREATE DATABASE or USE")
	}
	sort.Strings(references)
	for _, name := range references {
		key, err := n.key(name)
		if err != nil {
			return nil, err
		}
		canonical, ok := n.byKey[key]
		if !ok {
			canonical = name
			n.byKey[key] = name
		} else if _, declared := d.declared[canonical]; !declared && canonical != name {
			return nil, fmt.Errorf("database name collision: %q and %q without a unique CREATE DATABASE declaration with destination lower_case_table_names=%d", canonical, name, n.mode)
		}
		n.aliases[name] = canonical
	}
	for _, name := range n.byKey {
		n.names = append(n.names, name)
	}
	sort.Strings(n.names)

	// Resolve the database part before checking table declarations. A view's
	// placeholder and final declaration may use different database references,
	// but differently spelled TABLE/VIEW/SEQUENCE declarations still collide.
	tables := map[[2]string][2]string{}
	objects := make([][2]string, 0, len(d.tables))
	for name := range d.tables {
		objects = append(objects, name)
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i][0] != objects[j][0] {
			return objects[i][0] < objects[j][0]
		}
		return objects[i][1] < objects[j][1]
	})
	for _, name := range objects {
		table, err := n.key(name[1])
		if err != nil {
			return nil, err
		}
		key := [2]string{n.aliases[name[0]], table}
		if old, ok := tables[key]; ok && old[1] != name[1] {
			return nil, fmt.Errorf("table name collision: %q.%q and %q.%q with destination lower_case_table_names=%d", old[0], old[1], name[0], name[1], n.mode)
		}
		tables[key] = name
	}
	return n, nil
}

func quoteDatabaseComment(s string, noBackslash bool) string {
	if !noBackslash {
		s = strings.NewReplacer("\\", "\\\\", "\x00", "\\0", "\n", "\\n", "\r", "\\r", "\x1a", "\\Z").Replace(s)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
