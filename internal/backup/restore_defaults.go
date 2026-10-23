package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Use native CREATE DATABASE metadata, also present in external --databases
// dumps. Never infer a source collation from destination server defaults.
type databaseDefaults struct {
	CharacterSet string `json:"character_set"`
	Collation    string `json:"collation"`
}
type restoreDefaults struct {
	config              Config
	explicit            map[string]databaseDefaults
	declared            map[string]databaseDefaults
	used                map[string]bool
	required            map[databaseDefaults]bool
	onCreate            func(int64, int64) error
	comments            map[string]string
	currentDatabase     string
	tables              map[[2]string]bool
	lowerCaseTableNames int
	initialMode         string
	mode                dumpSQLMode
	charset             dumpCharset
}

func newRestoreDefaults(c Config) (*restoreDefaults, error) {
	d := &restoreDefaults{config: c, explicit: map[string]databaseDefaults{}, declared: map[string]databaseDefaults{}, used: map[string]bool{}, required: map[databaseDefaults]bool{}}
	d.comments = map[string]string{}
	d.tables = map[[2]string]bool{}
	d.mode.saved = map[string]string{}
	d.charset = dumpCharset{current: "utf8mb4", saved: map[string]string{}}
	if c.BackupMode != "combined" {
		d.currentDatabase = c.Database
	}
	if c.RestoreDatabaseDefaults != "" {
		dec := json.NewDecoder(strings.NewReader(c.RestoreDatabaseDefaults))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d.explicit); err != nil {
			return nil, fmt.Errorf("invalid RESTORE_DATABASE_DEFAULTS: %w", err)
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("RESTORE_DATABASE_DEFAULTS must contain one JSON object")
		}
		if len(d.explicit) == 0 {
			return nil, fmt.Errorf("RESTORE_DATABASE_DEFAULTS must be a nonempty JSON object")
		}
		for name, v := range d.explicit {
			if err := validateDatabases([]string{name}); err != nil {
				return nil, err
			}
			if !validCharsetName(v.CharacterSet) || !validCharsetName(v.Collation) {
				return nil, fmt.Errorf("RESTORE_DATABASE_DEFAULTS requires character_set and collation for %q", name)
			}
			d.explicit[name] = databaseDefaults{strings.ToLower(v.CharacterSet), strings.ToLower(v.Collation)}
		}
	}
	return d, nil
}
func validCharsetName(s string) bool {
	return s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
	}) < 0
}

// Tokenize only the small database/USE statements. Executable version comments
// in native dumps are inspected conservatively, irrespective of server version.
// Quoted names retain case and doubled backticks; routine bodies are not parsed.
type databaseSQLLexer struct {
	s           string
	quoted      bool
	quote       byte
	noBackslash bool
	ansiQuotes  bool
	charset     dumpCharset
}

func (l *databaseSQLLexer) next() (string, error) {
	l.quoted = false
	l.quote = 0
	for {
		l.s = strings.TrimSpace(l.s)
		if l.s == "" {
			return "", nil
		}
		if strings.HasPrefix(l.s, "--") || strings.HasPrefix(l.s, "#") {
			_, l.s, _ = strings.Cut(l.s, "\n")
			continue
		}
		if strings.HasPrefix(l.s, "/*!") || strings.HasPrefix(l.s, "/*M!") {
			n := 3
			if strings.HasPrefix(l.s, "/*M!") {
				n = 4
			}
			l.s = strings.TrimLeft(l.s[n:], "0123456789")
			continue
		}
		if strings.HasPrefix(l.s, "/*") {
			_, tail, ok := strings.Cut(l.s[2:], "*/")
			if !ok {
				return "", fmt.Errorf("unterminated database metadata comment")
			}
			l.s = tail
			continue
		}
		if strings.HasPrefix(l.s, "*/") {
			l.s = l.s[2:]
			continue
		}
		break
	}
	if strings.HasPrefix(l.s, "\\- enable the sandbox mode") {
		_, tail, ok := strings.Cut(l.s, "*/")
		if !ok {
			return "", fmt.Errorf("unterminated sandbox comment")
		}
		l.s = tail
		return l.next()
	}
	c := l.s[0]
	if c == '`' || c == '\'' || c == '"' {
		l.quoted = true
		l.quote = c
		var b strings.Builder
		for i := 1; i < len(l.s); i++ {
			// Use the same character boundaries as the packet scanner. A trail
			// byte can be a backtick or backslash in the dump's client charset.
			if n := l.charset.multiByteLen([]byte(l.s[i:min(i+2, len(l.s))])); n > 1 {
				b.WriteString(l.s[i : i+n])
				i += n - 1
				continue
			}
			if l.s[i] == c {
				if i+1 < len(l.s) && l.s[i+1] == c {
					b.WriteByte(c)
					i++
					continue
				}
				l.s = l.s[i+1:]
				return b.String(), nil
			}
			if l.s[i] == '\\' && c != '`' && !(c == '"' && l.ansiQuotes) && !l.noBackslash {
				i++
				if i == len(l.s) {
					return "", fmt.Errorf("unterminated database metadata string")
				}
				switch l.s[i] {
				case '0':
					b.WriteByte(0)
				case 'b':
					b.WriteByte('\b')
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case 'Z':
					b.WriteByte(26)
				case '_', '%':
					b.WriteByte('\\')
					b.WriteByte(l.s[i])
				default:
					b.WriteByte(l.s[i])
				}
				continue
			}
			b.WriteByte(l.s[i])
		}
		return "", fmt.Errorf("unterminated database metadata identifier")
	}
	if strings.ContainsRune("=;.(),@", rune(c)) {
		l.s = l.s[1:]
		return string(c), nil
	}
	i := 0
	for i < len(l.s) && l.s[i] > ' ' && !strings.ContainsRune("=;.(),@`\"'/*", rune(l.s[i])) {
		i++
	}
	if i == 0 {
		return "", fmt.Errorf("unsupported database metadata syntax")
	}
	token := l.s[:i]
	l.s = l.s[i:]
	return token, nil
}
func (d *restoreDefaults) inspect(raw string, offset, size int64) error {
	l := databaseSQLLexer{s: raw, noBackslash: d.mode.has("NO_BACKSLASH_ESCAPES"), ansiQuotes: d.mode.has("ANSI_QUOTES"), charset: d.charset}
	first, err := l.next()
	if err != nil {
		return err
	}
	if first == "" && int64(len(raw)) < size {
		return fmt.Errorf("statement prefix exceeds metadata inspection buffer")
	}
	first = strings.ToUpper(first)
	if first == "SET" {
		body, err := dumpStatementBody(raw)
		if err != nil {
			return err
		}
		if err := d.mode.update(body); err != nil {
			return err
		}
		return d.charset.update(body)
	}
	if first != "USE" && first != "CREATE" && first != "ALTER" && first != "DROP" {
		return nil
	}
	second, err := l.next()
	if err != nil {
		return err
	}
	if first == "USE" {
		if second == "" {
			return fmt.Errorf("missing database in USE")
		}
		d.used[second] = true
		d.currentDatabase = second
		return nil
	}
	if first == "CREATE" {
		if handled, err := d.inspectTable(second, l); handled || err != nil {
			return err
		}
	}
	if strings.EqualFold(second, "OR") {
		third, _ := l.next()
		fourth, _ := l.next()
		if strings.EqualFold(third, "REPLACE") && (strings.EqualFold(fourth, "DATABASE") || strings.EqualFold(fourth, "SCHEMA")) {
			return fmt.Errorf("CREATE OR REPLACE DATABASE is not supported during restore")
		}
		return nil
	}
	if !strings.EqualFold(second, "DATABASE") && !strings.EqualFold(second, "SCHEMA") {
		return nil
	}
	if first == "DROP" {
		return fmt.Errorf("DROP DATABASE is not supported during restore")
	}
	conditionalOffset := offset + int64(len(raw)-len(l.s))
	name, err := l.next()
	if err != nil {
		return err
	}
	conditional := strings.EqualFold(name, "IF") && !l.quoted
	if conditional {
		not, _ := l.next()
		exists, _ := l.next()
		if first != "CREATE" || !strings.EqualFold(not, "NOT") || !strings.EqualFold(exists, "EXISTS") {
			return fmt.Errorf("unsupported database metadata syntax")
		}
		name, err = l.next()
		if err != nil {
			return err
		}
	}
	if err := validateDatabases([]string{name}); err != nil {
		return err
	}
	if int64(len(raw)) < size {
		return fmt.Errorf("database metadata exceeds inspection buffer")
	}
	v := databaseDefaults{}
	var comment string
	for {
		token, err := l.next()
		if err != nil {
			return err
		}
		if token == "" || token == ";" {
			break
		}
		switch strings.ToUpper(token) {
		case "DEFAULT":
			continue
		case "CHARACTER":
			set, _ := l.next()
			if !strings.EqualFold(set, "SET") {
				return fmt.Errorf("unsupported CHARACTER clause in database metadata")
			}
			value, err := databaseOptionValue(&l)
			if err != nil {
				return err
			}
			v.CharacterSet = value
		case "CHARSET":
			value, err := databaseOptionValue(&l)
			if err != nil {
				return err
			}
			v.CharacterSet = value
		case "COMMENT":
			value, err := l.next()
			if err != nil {
				return err
			}
			if value == "=" && !l.quoted {
				value, err = l.next()
			}
			if err != nil {
				return err
			}
			if l.quote != '\'' && (l.quote != '"' || l.ansiQuotes) {
				return fmt.Errorf("database COMMENT requires a string literal")
			}
			if d.charset.current != "utf8mb4" && d.charset.current != "utf8" && d.charset.current != "utf8mb3" {
				return fmt.Errorf("database COMMENT requires UTF-8 metadata")
			}
			comment = value
		case "COLLATE":
			value, err := databaseOptionValue(&l)
			if err != nil {
				return err
			}
			v.Collation = value
		default:
			return fmt.Errorf("unsupported database option %q", token)
		}
	}
	d.used[name] = true
	d.required[v] = true
	if first == "CREATE" && !conditional && d.onCreate != nil {
		if err := d.onCreate(conditionalOffset, size); err != nil {
			return err
		}
	}
	if first == "CREATE" {
		if old, ok := d.declared[name]; ok && old != v {
			return fmt.Errorf("conflicting database defaults for %q", name)
		}
		if old, ok := d.comments[name]; ok && old != comment {
			return fmt.Errorf("conflicting database comments for %q", name)
		}
		d.declared[name] = v
		// CREATE DATABASE without COMMENT declares the empty default. Native
		// dumps omit empty comments; ALTER must still clear an existing value.
		// Dumps without CREATE metadata leave the destination comment alone.
		d.comments[name] = comment
	}
	return nil
}
func databaseOptionValue(l *databaseSQLLexer) (string, error) {
	v, err := l.next()
	if err != nil {
		return "", err
	}
	if v == "=" {
		v, err = l.next()
		if err != nil {
			return "", err
		}
	}
	if !validCharsetName(v) {
		return "", fmt.Errorf("invalid character set or collation in database metadata")
	}
	return strings.ToLower(v), nil
}
func quoteDatabase(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// A plan is built before any destination SQL is executed. All metadata and
// configuration references use the same resolved database identities.
type restoreDatabase struct {
	name     string
	defaults databaseDefaults
	comment  *string
}

type restorePlan struct {
	databases []restoreDatabase
	selected  string
	required  map[databaseDefaults]bool
}

func (d *restoreDefaults) plan() (*restorePlan, error) {
	names, err := d.resolveNames()
	if err != nil {
		return nil, err
	}
	p := &restorePlan{required: map[databaseDefaults]bool{}}
	for v := range d.required {
		p.required[v] = true
	}
	if d.config.BackupMode != "combined" {
		p.selected = names.aliases[d.config.Database]
		for _, name := range names.names {
			if name != p.selected {
				return nil, fmt.Errorf("dump database %q differs from MARIADB_DATABASE %q", name, d.config.Database)
			}
		}
	}
	explicit := map[string]databaseDefaults{}
	keys := make([]string, 0, len(d.explicit))
	for name := range d.explicit {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		key, err := names.key(name)
		if err != nil {
			return nil, err
		}
		canonical, ok := names.byKey[key]
		if !ok {
			return nil, fmt.Errorf("RESTORE_DATABASE_DEFAULTS contains unused database %q", name)
		}
		v := d.explicit[name]
		if old, ok := explicit[canonical]; ok && old != v {
			return nil, fmt.Errorf("RESTORE_DATABASE_DEFAULTS has conflicting aliases for database %q", canonical)
		}
		explicit[canonical] = v
	}
	for _, name := range names.names {
		v := d.declared[name]
		if override, ok := explicit[name]; ok {
			if v.CharacterSet != "" && v.CharacterSet != override.CharacterSet || v.Collation != "" && v.Collation != override.Collation {
				return nil, fmt.Errorf("RESTORE_DATABASE_DEFAULTS conflicts with dump metadata for %q", name)
			}
			v = override
		}
		if v.CharacterSet == "" || v.Collation == "" {
			return nil, fmt.Errorf("missing source database defaults for %q; supply RESTORE_DATABASE_DEFAULTS with character_set and collation", name)
		}
		database := restoreDatabase{name: name, defaults: v}
		if comment, ok := d.comments[name]; ok {
			database.comment = &comment
		}
		p.databases = append(p.databases, database)
		p.required[v] = true
	}
	return p, nil
}

func (d *restoreDefaults) prepare(ctx context.Context, a *App, runner commandRunner, limit int64) (string, error) {
	plan, err := d.plan()
	if err != nil {
		return "", err
	}
	var output bytes.Buffer
	args := append(a.connectionArgs(), "--batch", "--raw", "--skip-column-names", "--execute=SELECT COLLATION_NAME, CHARACTER_SET_NAME FROM information_schema.COLLATION_CHARACTER_SET_APPLICABILITY UNION SELECT FULL_COLLATION_NAME, CHARACTER_SET_NAME FROM information_schema.COLLATION_CHARACTER_SET_APPLICABILITY")
	if err := runner.run(ctx, &output, "mariadb", args...); err != nil {
		return "", fmt.Errorf("could not check destination collations: %w", err)
	}
	available := map[databaseDefaults]bool{}
	collations := map[string]bool{}
	charsets := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		coll, cs, ok := strings.Cut(line, "\t")
		if !ok || !validCharsetName(coll) || !validCharsetName(cs) {
			return "", fmt.Errorf("invalid destination collation result")
		}
		available[databaseDefaults{strings.ToLower(cs), strings.ToLower(coll)}] = true
		collations[strings.ToLower(coll)] = true
		charsets[strings.ToLower(cs)] = true
	}
	for v := range plan.required {
		if v.CharacterSet != "" && !charsets[v.CharacterSet] {
			return "", fmt.Errorf("source character set %q is unavailable on destination", v.CharacterSet)
		}
		if v.Collation != "" {
			if !collations[v.Collation] {
				return "", fmt.Errorf("source collation %q is unavailable on destination", v.Collation)
			}
			if v.CharacterSet != "" && !available[v] {
				return "", fmt.Errorf("collation %q does not belong to character set %q", v.Collation, v.CharacterSet)
			}
		}
	}
	var prefix strings.Builder
	for _, database := range plan.databases {
		v := database.defaults
		options := " CHARACTER SET " + v.CharacterSet + " COLLATE " + v.Collation
		if database.comment != nil {
			options += " COMMENT " + quoteDatabaseComment(*database.comment, strings.Contains(","+strings.ToUpper(d.initialMode)+",", ",NO_BACKSLASH_ESCAPES,"))
		}
		options += ";\n"
		prefix.WriteString("CREATE DATABASE IF NOT EXISTS " + quoteDatabase(database.name) + options)
		prefix.WriteString("ALTER DATABASE " + quoteDatabase(database.name) + options)
	}
	if plan.selected != "" {
		prefix.WriteString("USE " + quoteDatabase(plan.selected) + ";\n")
	}
	if err := checkDumpPackets(ctx, strings.NewReader(prefix.String()), limit, d.initialMode); err != nil {
		return "", err
	}
	return prefix.String(), nil
}
