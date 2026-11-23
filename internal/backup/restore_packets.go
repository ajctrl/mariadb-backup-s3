package backup

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const maxRestorePacket = int64(1 << 30)

// Count a conservative upper bound on each client statement, including comments,
// whitespace that the client may remove. Do not split on quoted
// delimiters, comments, or semicolons inside DELIMITER-delimited routines. Memory
// use is bounded even for a single very large INSERT. This reads mariadb-dump
// output, not arbitrary scripts with client commands or dynamically built SQL.
func checkDumpPackets(ctx context.Context, input io.Reader, limit int64, initialMode string) error {
	return scanDumpPackets(ctx, input, limit, initialMode, nil)
}

func scanDumpPackets(ctx context.Context, input io.Reader, limit int64, initialMode string, inspect func(string, int64, int64, dumpCharset, packetQuoteRules) error) error {
	r := bufio.NewReaderSize(input, 64*1024)
	delimiter := []byte(";")
	var quote byte
	var blockComment, lineComment, escaped, hasSQL bool
	lineStart := true
	var size, processed, nextContextCheck int64
	line, statementLine := 1, 1
	var prefix []byte
	mode := dumpSQLMode{current: strings.ToUpper(initialMode), saved: make(map[string]string), rules: quoteRules(initialMode)}
	charset := dumpCharset{current: "utf8mb4", saved: make(map[string]string)}
	finish := func(terminated bool) error {
		if inspect != nil {
			start := processed - size
			if terminated {
				start -= int64(len(delimiter))
			}
			if err := inspect(string(prefix), start, size, charset, mode.rules); err != nil {
				return fmt.Errorf("line %d: %w", statementLine, err)
			}
		}
		statement, err := dumpStatementBody(string(prefix))
		if err != nil {
			return fmt.Errorf("line %d: %w", statementLine, err)
		}
		if err := mode.update(statement); err != nil {
			return fmt.Errorf("line %d: %w", statementLine, err)
		}
		if err := charset.update(statement); err != nil {
			return fmt.Errorf("line %d: %w", statementLine, err)
		}
		size, prefix = 0, prefix[:0]
		hasSQL = false
		statementLine = line
		return nil
	}
	consume := func(n int) error {
		for range n {
			b, err := r.ReadByte()
			if err != nil {
				return err
			}
			size++
			processed++
			if len(prefix) < 64*1024 {
				prefix = append(prefix, b)
			}
			lineStart = b == '\n'
			if lineStart {
				line++
			}
			// Reserve the COM_QUERY byte and a terminator. Comments
			// make this an upper bound, so a boundary case can be rejected early.
			if limit > 0 && size+2 > limit {
				return fmt.Errorf("SQL near line %d needs a packet larger than max_allowed_packet=%d bytes; increase the destination server's max_allowed_packet and retry (client limit: %d bytes)", statementLine, limit, maxRestorePacket)
			}
		}
		return nil
	}
	for {
		if processed >= nextContextCheck {
			if err := ctx.Err(); err != nil {
				return err
			}
			nextContextCheck = processed + 65536
		}
		peek, err := r.Peek(max(4, len(delimiter)))
		if err != nil && err != io.EOF {
			return err
		}
		if len(peek) == 0 {
			if quote != 0 || blockComment {
				return fmt.Errorf("unterminated SQL quote or comment near line %d", statementLine)
			}
			return finish(false)
		}
		if lineStart && quote == 0 && !blockComment {
			// mariadb-dump emits DELIMITER on its own short line. Read only a
			// bounded prefix here, never an entire potentially huge INSERT line.
			start, _ := r.Peek(256)
			end := bytes.IndexByte(start, '\n')
			if end < 0 {
				end = len(start)
			}
			fields := strings.Fields(string(start[:end]))
			if len(fields) > 0 && strings.EqualFold(fields[0], "DELIMITER") {
				if hasSQL || len(fields) != 2 || len(fields[1]) > 64 || end == 256 || strings.IndexFunc(fields[1], func(r rune) bool { return r < 33 || r > 126 || r == '\\' }) >= 0 {
					return fmt.Errorf("invalid DELIMITER directive at line %d", line)
				}
				if err := finish(false); err != nil {
					return err
				}
				delimiter = []byte(fields[1])
				// Directive bytes are client instructions, not server packets.
				if _, err := r.Discard(end); err != nil {
					return err
				}
				processed += int64(end)
				lineStart = false
				continue
			}
			// Peek(256) may refill/compact the reader and invalidate the
			// earlier slice, especially at a line near a buffer boundary.
			peek, err = r.Peek(max(4, len(delimiter)))
			if err != nil && err != io.EOF {
				return err
			}
		}
		b, n := peek[0], 1
		switch {
		case lineComment:
			lineComment = b != '\n'
		case blockComment:
			if bytes.HasPrefix(peek, []byte("*/")) {
				blockComment, n = false, 2
			}
		case !escaped && charset.multiByteLen(peek) > 1:
			// A trail byte in SJIS/CP932, Big5 or GBK can be '\\' or
			// '`'. Consume the complete character before SQL punctuation.
			n = charset.multiByteLen(peek)
			hasSQL = true
		case quote != 0:
			if escaped {
				escaped = false
			} else if b == '\\' && quote != '`' && !(quote == '"' && mode.rules.ansiQuotes) && !mode.rules.noBackslash {
				escaped = true
			} else if b == quote {
				if len(peek) > 1 && peek[1] == quote {
					n = 2
				} else {
					quote = 0
				}
			}
		case bytes.HasPrefix(peek, delimiter):
			if _, err := r.Discard(len(delimiter)); err != nil {
				return err
			}
			processed += int64(len(delimiter))
			lineStart = false
			if err := finish(true); err != nil {
				return err
			}
			continue
		case bytes.HasPrefix(peek, []byte("/*")):
			// The MariaDB client parses quotes/delimiters inside executable
			// comments too. Treating them as ordinary comments can split on a
			// delimiter following a quoted "*/" inside a routine body.
			executable := bytes.HasPrefix(peek, []byte("/*!")) || bytes.HasPrefix(peek, []byte("/*M!"))
			blockComment, n = !executable, 2
			hasSQL = hasSQL || executable
		case b == '#' || len(peek) >= 3 && peek[0] == '-' && peek[1] == '-' && peek[2] <= ' ':
			lineComment = true
		case b == '\'' || b == '"' || b == '`':
			quote = b
			hasSQL = true
		default:
			hasSQL = hasSQL || b > ' '
		}
		if err := consume(n); err != nil {
			return err
		}
	}
}

type dumpSQLMode struct {
	rules   packetQuoteRules
	current string
	saved   map[string]string
}

var (
	dumpSaveMode = regexp.MustCompile(`(?is)^SET\s+(@[A-Z_]+)\s*=\s*@@SQL_MODE\s*(?:,\s*SQL_MODE\s*=\s*'([A-Z0-9_,]*)')?$`)
	dumpSetMode  = regexp.MustCompile(`(?is)^SET\s+SQL_MODE\s*=\s*(?:'([A-Z0-9_,]*)'|(@[A-Z_]+))$`)
)

// Strip leading comments/version wrappers for the small session-state
// statements emitted by mariadb-dump. The returned text is only for inspection;
// the original SQL bytes are never rewritten or uppercased during import.
func dumpStatementBody(statement string) (string, error) {
	s := strings.TrimSpace(statement)
	for len(s) > 0 {
		if strings.HasPrefix(s, "--") || strings.HasPrefix(s, "#") {
			_, s, _ = strings.Cut(s, "\n")
		} else if strings.HasPrefix(s, "/*") {
			body, tail, ok := strings.Cut(s[2:], "*/")
			if !ok {
				// A large executable CREATE statement cannot change the session
				// mode during creation. No need to buffer its whole routine body.
				start := strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(strings.TrimPrefix(s[2:], "M"), "!"), "0123456789"))
				if strings.HasPrefix(strings.ToUpper(start), "CREATE ") {
					return "", nil
				}
				return "", fmt.Errorf("comment exceeds session state inspection buffer")
			}
			body = strings.TrimPrefix(body, "M")
			if strings.HasPrefix(body, "!") && !strings.Contains(body, "\\- enable the sandbox mode") {
				body = strings.TrimLeft(body[1:], "0123456789")
				s = body + tail
			} else {
				s = tail
			}
		} else {
			break
		}
		s = strings.TrimSpace(s)
	}
	return strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(s, ";"))), nil
}

// Track the save/set/restore SQL_MODE statements, including per-routine modes.
// Guessing an unknown expression could underestimate statement sizes.
func (m *dumpSQLMode) update(s string) error {
	if !strings.HasPrefix(s, "SET ") || !strings.Contains(s, "SQL_MODE") {
		return nil
	}
	if match := dumpSaveMode.FindStringSubmatch(s); match != nil {
		m.saved[match[1]] = m.current
		if strings.Contains(s, ",") {
			m.current = match[2]
			m.rules = quoteRules(m.current)
		}
		return nil
	}
	if match := dumpSetMode.FindStringSubmatch(s); match != nil {
		if match[2] == "" {
			m.current = match[1]
		} else if saved, ok := m.saved[match[2]]; ok {
			m.current = saved
		} else {
			return fmt.Errorf("unknown saved SQL_MODE in dump")
		}
		m.rules = quoteRules(m.current)
		return nil
	}
	return fmt.Errorf("unsupported SQL_MODE assignment in dump")
}
