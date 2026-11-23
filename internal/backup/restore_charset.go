package backup

import (
	"fmt"
	"regexp"
	"strings"
)

// mariadb-dump emits routines in their original character_set_client, even
// with --default-character-set=utf8mb4. SET changes the server's charset but
// not the mariadb client's lexer; both must switch together during restore.
type dumpCharset struct {
	current string
	saved   map[string]string
}

var (
	dumpSaveCharset = regexp.MustCompile(`^SET\s+(@[A-Z_]+)\s*=\s*@@CHARACTER_SET_CLIENT$`)
	dumpSetCharset  = regexp.MustCompile(`^SET\s+CHARACTER_SET_CLIENT\s*=\s*(?:'([A-Z0-9_]+)'|([A-Z0-9_]+)|(@[A-Z_]+))$`)
	dumpSetNames    = regexp.MustCompile(`^SET\s+NAMES\s+(?:'([A-Z0-9_]+)'|([A-Z0-9_]+))(?:\s+COLLATE\s+[A-Z0-9_]+)?$`)
)

func (c *dumpCharset) update(s string) error {
	if !strings.HasPrefix(s, "SET ") {
		return nil
	}
	if match := dumpSaveCharset.FindStringSubmatch(s); match != nil {
		c.saved[match[1]] = c.current
		return nil
	}
	var next string
	if match := dumpSetCharset.FindStringSubmatch(s); match != nil {
		next = match[1] + match[2]
		if match[3] != "" {
			var ok bool
			next, ok = c.saved[match[3]]
			if !ok {
				return fmt.Errorf("unknown saved CHARACTER_SET_CLIENT in dump")
			}
		}
	} else if match := dumpSetNames.FindStringSubmatch(s); match != nil {
		next = match[1] + match[2]
	} else if strings.Contains(s, "CHARACTER_SET_CLIENT") || strings.HasPrefix(s, "SET NAMES") || strings.HasPrefix(s, "SET CHARACTER SET") {
		return fmt.Errorf("unsupported character set assignment in dump")
	} else {
		return nil
	}
	next = strings.ToLower(next)
	// These are the ASCII-compatible client charsets in MariaDB 11.4.
	// UCS2/UTF16/UTF32 cannot be used by the mariadb client. Never guess
	// the lexical rules for an unknown charset or interpolate arbitrary SQL.
	switch next {
	case "armscii8", "ascii", "big5", "binary", "cp1250", "cp1251", "cp1256", "cp1257",
		"cp850", "cp852", "cp866", "cp932", "dec8", "eucjpms", "euckr", "gb2312", "gbk",
		"geostd8", "greek", "hebrew", "hp8", "keybcs2", "koi8r", "koi8u", "latin1",
		"latin2", "latin5", "latin7", "macce", "macroman", "sjis", "swe7", "tis620",
		"ujis", "utf8", "utf8mb3", "utf8mb4":
		c.current = next
		return nil
	default:
		return fmt.Errorf("unsupported client character set in dump: %s", next)
	}
}

// These encodings can contain ASCII bytes as trail bytes. The byte ranges
// match MariaDB 11.4 strings/ctype-{sjis,cp932,big5,gbk,euc_kr}.c, not Unicode conversion:
// do not transcode data or replace undecodable bytes.
func (c *dumpCharset) multiByteLen(p []byte) int {
	if len(p) < 2 {
		return 1
	}
	a, b := p[0], p[1]
	switch c.current {
	case "sjis", "cp932":
		if (a >= 0x81 && a <= 0x9f || a >= 0xe0 && a <= 0xfc) &&
			(b >= 0x40 && b <= 0x7e || b >= 0x80 && b <= 0xfc) {
			return 2
		}
	case "big5":
		if a >= 0xa1 && a <= 0xf9 && (b >= 0x40 && b <= 0x7e || b >= 0xa1 && b <= 0xfe) {
			return 2
		}
	case "gbk":
		if a >= 0x81 && a <= 0xfe && b >= 0x40 && b <= 0xfe && b != 0x7f {
			return 2
		}
	case "euckr":
		if a >= 0x81 && a <= 0xfe && (b >= 0x41 && b <= 0x5a || b >= 0x61 && b <= 0x7a || b >= 0x81 && b <= 0xfe) {
			return 2
		}
	}
	return 1
}
