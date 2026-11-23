package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Use native CREATE DATABASE metadata, also present in external --databases
// dumps. Never infer a source collation from destination server defaults.
type databaseDefaults struct {
	CharacterSet string `json:"character_set"`
	Collation    string `json:"collation"`
}
type restoreDefaults struct {
	name        string
	foldNames   bool
	query       func(string) (string, error)
	aliases     map[string]bool
	source      databaseDefaults
	declared    bool
	explicit    *databaseDefaults
	comment     string
	destination *databaseDefaults
	file        *os.File
}

func newRestoreDefaults(c Config, foldNames bool, destination *databaseDefaults, file *os.File, query func(string) (string, error)) (*restoreDefaults, error) {
	d := &restoreDefaults{name: c.Database, foldNames: foldNames, destination: destination, file: file, query: query, aliases: make(map[string]bool)}
	if c.RestoreDatabaseDefaults == "" {
		return d, nil
	}
	dec := json.NewDecoder(strings.NewReader(c.RestoreDatabaseDefaults))
	dec.DisallowUnknownFields()
	var settings map[string]databaseDefaults
	if err := dec.Decode(&settings); err != nil {
		return nil, fmt.Errorf("invalid RESTORE_DATABASE_DEFAULTS: %w", err)
	}
	var extra any
	if len(settings) != 1 || dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("RESTORE_DATABASE_DEFAULTS must contain one entry for %q", c.Database)
	}
	for name, v := range settings {
		ok, err := d.matches(name)
		if err != nil {
			return nil, err
		}
		if !ok || !validCharsetName(v.CharacterSet) || !validCharsetName(v.Collation) {
			return nil, fmt.Errorf("RESTORE_DATABASE_DEFAULTS requires character_set and collation for %q", c.Database)
		}
		d.explicit = &databaseDefaults{strings.ToLower(v.CharacterSet), strings.ToLower(v.Collation)}
	}
	return d, nil
}

func (d *restoreDefaults) matches(name string) (bool, error) {
	if name == d.name {
		return true, nil
	}
	if !d.foldNames {
		return false, nil
	}
	if strings.IndexFunc(name+d.name, func(r rune) bool { return r > 127 }) < 0 {
		return strings.EqualFold(name, d.name), nil
	}
	if same, ok := d.aliases[name]; ok {
		return same, nil
	}
	// Use the server's identifier case mapping for non-ASCII aliases.
	row, err := d.query("SELECT BINARY LOWER(" + sqlText(name) + ") = BINARY LOWER(" + sqlText(d.name) + ")")
	if err != nil {
		return false, err
	}
	if row != "0" && row != "1" {
		return false, fmt.Errorf("invalid database name comparison result")
	}
	d.aliases[name] = row == "1"
	return d.aliases[name], nil
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
func (d *restoreDefaults) inspect(raw string, offset, size int64, charset dumpCharset, rules packetQuoteRules) error {
	l := databaseSQLLexer{s: raw, noBackslash: rules.noBackslash, ansiQuotes: rules.ansiQuotes, charset: charset}
	first, err := l.next()
	if err != nil {
		return err
	}
	if first == "" && int64(len(raw)) < size {
		return fmt.Errorf("statement prefix exceeds metadata inspection buffer")
	}
	keywordOffset := len(raw) - len(l.s) - len(first)
	first = strings.ToUpper(first)
	if first != "USE" && first != "CREATE" && first != "ALTER" && first != "DROP" {
		return nil
	}
	second, err := l.next()
	if err != nil {
		return err
	}
	if first == "USE" {
		ok, err := d.matches(second)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("dump selects database %q, expected %q", second, d.name)
		}
		return nil
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
	ok, err := d.matches(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("dump database %q differs from MARIADB_DATABASE %q", name, d.name)
	}
	if int64(len(raw)) < size {
		return fmt.Errorf("database metadata exceeds inspection buffer")
	}
	v := databaseDefaults{}
	var comment string
	var hasComment bool
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
			if charset.current != "utf8mb4" && charset.current != "utf8" && charset.current != "utf8mb3" {
				return fmt.Errorf("database COMMENT requires UTF-8 metadata")
			}
			comment = value
			hasComment = true
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
	if first == "ALTER" {
		if d.destination == nil {
			// A new database can follow the dump's original routine-specific defaults.
			return nil
		}
		if hasComment || v.differs(*d.destination) {
			return fmt.Errorf("dump requests ALTER DATABASE %q with different defaults or a comment; existing database left unchanged (destination %s, dump %s)", name, *d.destination, v)
		}
	} else {
		if d.declared && (d.source != v || d.comment != comment) {
			return fmt.Errorf("conflicting CREATE DATABASE metadata for %q", name)
		}
		d.source, d.comment, d.declared = v, comment, true
	}
	// Handle database creation once, after preflight. Neutralize redundant DDL
	// only in the private download and preserve line numbers for diagnostics.
	masked := []byte(raw)
	for i, b := range masked {
		if b != '\n' && b != '\r' {
			masked[i] = ' '
		}
	}
	const sandbox = "/*M!999999\\- enable the sandbox mode */"
	if pos := strings.Index(raw[:keywordOffset], sandbox); pos >= 0 {
		copy(masked[pos:], sandbox)
	}
	copy(masked[keywordOffset:], "DO 0")
	_, err = d.file.WriteAt(masked, offset)
	return err
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

func (v databaseDefaults) String() string {
	return fmt.Sprintf("character_set=%q, collation=%q", v.CharacterSet, v.Collation)
}
func (v databaseDefaults) differs(other databaseDefaults) bool {
	return v.CharacterSet != "" && v.CharacterSet != other.CharacterSet || v.Collation != "" && v.Collation != other.Collation
}

// The lookup and import use the same session that owns the job lock.
func destinationDefaults(query func(string) (string, error), database string) (*databaseDefaults, error) {
	name := sqlText(database)
	sql := "SELECT COALESCE((SELECT CONCAT(DEFAULT_CHARACTER_SET_NAME, '/', DEFAULT_COLLATION_NAME) FROM information_schema.SCHEMATA WHERE BINARY IF(@@lower_case_table_names=0,SCHEMA_NAME,LOWER(SCHEMA_NAME)) = BINARY IF(@@lower_case_table_names=0," + name + ",LOWER(" + name + "))), '-')"
	row, err := query(sql)
	if err != nil {
		return nil, fmt.Errorf("read destination database defaults: %w", err)
	}
	if row == "-" {
		return nil, nil
	}
	cs, coll, ok := strings.Cut(row, "/")
	if !ok || !validCharsetName(cs) || !validCharsetName(coll) {
		return nil, fmt.Errorf("invalid destination database defaults for %q", database)
	}
	return &databaseDefaults{strings.ToLower(cs), strings.ToLower(coll)}, nil
}

func (a *App) prepareDatabase(ctx context.Context, file *os.File, destination *databaseDefaults, foldNames bool, limit int64, mode string, query func(string) (string, error)) (string, error) {
	d, err := newRestoreDefaults(a.Config, foldNames, destination, file, query)
	if err != nil {
		return "", err
	}
	// The local packet preflight has already measured this file using MariaDB client semantics.
	if err := scanDumpPackets(ctx, file, 0, mode, d.inspect); err != nil {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	v := d.source
	if d.explicit != nil {
		if v.differs(*d.explicit) {
			return "", fmt.Errorf("RESTORE_DATABASE_DEFAULTS conflicts with dump metadata for %q", d.name)
		}
		v = *d.explicit
	}
	name := quoteDatabase(d.name)
	prefix := ""
	complete := v.CharacterSet != "" && v.Collation != ""
	if destination == nil {
		if !complete {
			return "", fmt.Errorf("database %q is missing and source defaults are incomplete; set RESTORE_DATABASE_DEFAULTS", d.name)
		}
		// If another client created the database since the lookup, fail before import.
		prefix = "CREATE DATABASE " + name + " CHARACTER SET " + v.CharacterSet + " COLLATE " + v.Collation
		if d.comment != "" {
			prefix += " COMMENT " + quoteDatabaseComment(d.comment, quoteRules(mode).noBackslash)
		}
		prefix += ";\n"
	} else {
		if v.differs(*destination) {
			return "", fmt.Errorf("database defaults differ for %q: source %s; destination %s; no SQL imported", d.name, v, *destination)
		}
		if !complete {
			fmt.Fprintf(a.Err, "Warning: source database defaults for %q could not be fully verified; restoring with existing %s.\n", d.name, *destination)
		}
	}
	prefix += "USE " + name + ";\n"
	if err := checkDumpPackets(ctx, strings.NewReader(prefix), limit, mode); err != nil {
		return "", err
	}
	return prefix, nil
}

func quoteDatabaseComment(s string, noBackslash bool) string {
	if !noBackslash {
		s = strings.NewReplacer("\\", "\\\\", "\x00", "\\0", "\n", "\\n", "\r", "\\r", "\x1a", "\\Z").Replace(s)
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
