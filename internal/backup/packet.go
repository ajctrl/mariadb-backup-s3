package backup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
)

// SQL text plus the COM_QUERY command byte must fit the client's and
// server's packet buffers. Account for the command byte and buffer terminator.
func checkPacketSize(ctx context.Context, f *os.File, limit int64, sqlMode string) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size()+1 < limit {
		return nil
	}
	largest, err := largestStatement(ctx, f, sqlMode)
	if err != nil {
		return err
	}
	if largest+1 >= limit {
		return fmt.Errorf("restore needs max_allowed_packet of at least %d bytes (session limit %d); no SQL imported", largest+2, limit)
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

func largestStatement(ctx context.Context, src io.Reader, sqlMode string) (int64, error) {
	r := bufio.NewReaderSize(src, 64<<10)
	delimiter := []byte(";")
	mode := quoteRules(sqlMode)
	savedModes := make(map[string]packetQuoteRules)
	statementStart := true
	var quote byte
	var lineComment, blockComment bool
	lineStart := true
	var size, largest int64
	for {
		if r.Buffered() == 0 && ctx.Err() != nil {
			return 0, ctx.Err()
		}
		c, err := r.ReadByte()
		if errors.Is(err, io.EOF) {
			return max(size, largest), nil
		}
		if err != nil {
			return 0, err
		}
		size++
		start := lineStart
		lineStart = c == '\n' || (start && (c == ' ' || c == '\t' || c == '\r'))
		if lineComment {
			size--
			if c == '\n' {
				lineComment = false
				if !statementStart {
					size++
				}
			}
			continue
		}
		if blockComment {
			size--
			if c == '*' {
				if b, _ := r.Peek(1); bytes.Equal(b, []byte("/")) {
					r.Discard(1)
					if !statementStart {
						size++
					}
					blockComment = false
				}
			}
			continue
		}
		if quote != 0 {
			if c == '\\' && quote != '`' && !mode.noBackslash && (quote != '"' || !mode.ansiQuotes) {
				if _, err := r.ReadByte(); err == nil {
					size++
				} else if err != io.EOF {
					return 0, err
				}
			} else if c == quote {
				if b, _ := r.Peek(1); len(b) > 0 && b[0] == quote {
					r.Discard(1)
					size++
				} else {
					quote = 0
				}
			}
			continue
		}
		if statementStart && start && (c == 'd' || c == 'D') {
			b, _ := r.Peek(9)
			if len(b) >= 9 && bytes.EqualFold(b[:8], []byte("elimiter")) && (b[8] == ' ' || b[8] == '\t') {
				// MariaDB's DELIMITER command uses at most 15 bytes.
				line, err := r.ReadSlice('\n')
				if err != nil && err != io.EOF {
					return 0, fmt.Errorf("invalid DELIMITER directive: %w", err)
				}
				fields := strings.Fields(string(line))
				if len(fields) != 2 || len(fields[1]) > 15 || strings.ContainsAny(fields[1], "\\'\"") {
					return 0, errors.New("unsupported DELIMITER directive in restore SQL")
				}
				delimiter = []byte(fields[1])
				size--
				lineStart = true
				continue
			}
		}
		if c == delimiter[0] {
			b, _ := r.Peek(len(delimiter) - 1)
			if bytes.Equal(b, delimiter[1:]) {
				r.Discard(len(b))
				largest = max(largest, size-1)
				size = 0
				statementStart = true
				continue
			}
		}
		// mariadb-dump saves and switches SQL_MODE around stored definitions.
		// Inspect the bounded SET header before parsing quotes under the new mode.
		if statementStart && (c == 's' || c == 'S' || c == '/') {
			next, _ := r.Peek(1024)
			if err := applyDumpSQLMode(c, next, &mode, savedModes); err != nil {
				return 0, err
			}
		}
		switch c {
		case ' ', '\t', '\n', '\r':
			if statementStart {
				size--
			}
			continue
		case '\'', '"', '`':
			statementStart = false
			quote = c
		case '#':
			size--
			lineComment = true
		case '-':
			b, _ := r.Peek(2)
			lineComment = len(b) >= 1 && b[0] == '-' && (statementStart || len(b) == 2 && b[1] <= 32)
			if lineComment {
				size--
			} else {
				statementStart = false
			}
		case '/':
			b, _ := r.Peek(3)
			if len(b) > 0 && b[0] == '*' && !bytes.HasPrefix(b, []byte("*!")) && !bytes.HasPrefix(b, []byte("*M!")) {
				r.Discard(1)
				size--
				blockComment = true
			} else {
				statementStart = false
			}
		default:
			statementStart = false
		}
	}
}

// These are the SQL_MODE assignments emitted by mariadb-dump, including
// saving and restoring the session mode around each stored routine.
type packetQuoteRules struct{ noBackslash, ansiQuotes bool }

func quoteRules(raw string) packetQuoteRules {
	modes := strings.Split(strings.ToUpper(raw), ",")
	return packetQuoteRules{slices.Contains(modes, "NO_BACKSLASH_ESCAPES"), slices.ContainsFunc(modes, func(m string) bool {
		switch m {
		case "ANSI_QUOTES", "ANSI", "DB2", "MAXDB", "MSSQL", "ORACLE", "POSTGRESQL":
			return true
		}
		return false
	})}
}

var modeSet = regexp.MustCompile(`(?i)^(?:/\*M?![0-9]*\s*)?SET\s+(?:(?:SESSION|LOCAL)\s+)?(?:@@(?:SESSION\.)?)?(?:SQL_MODE|@[A-Z0-9_]+\s*=\s*@@SQL_MODE\b)`)
var modeAssignment = regexp.MustCompile(`(?i)^(?:/\*M?![0-9]*\s*)?SET\s+(?:(?:SESSION|LOCAL)\s+)?(?:(@[A-Z0-9_]+)\s*=\s*@@SQL_MODE\s*(?:,\s*)?)?(?:(?:@@(?:SESSION\.)?)?SQL_MODE\s*=\s*('[A-Z0-9_,]*'|@[A-Z0-9_]+))?\s*(?:\*/|;|$)`)

func applyDumpSQLMode(first byte, next []byte, mode *packetQuoteRules, saved map[string]packetQuoteRules) error {
	if first == '/' && !bytes.HasPrefix(next, []byte("*!")) && !bytes.HasPrefix(next, []byte("*M!")) {
		return nil
	}
	if (first == 'S' || first == 's') && (len(next) < 3 || !bytes.EqualFold(next[:2], []byte("et"))) {
		return nil
	}
	head := append([]byte{first}, next...)
	if !modeSet.Match(head) {
		return nil
	}
	fields := modeAssignment.FindSubmatch(head)
	if fields == nil {
		return errors.New("cannot verify packet sizes for this SQL_MODE assignment")
	}
	if len(fields[1]) > 0 {
		saved[strings.ToUpper(string(fields[1]))] = *mode
	}
	if len(fields[2]) == 0 {
		return nil
	}
	value := string(fields[2])
	if value[0] == '@' {
		var ok bool
		*mode, ok = saved[strings.ToUpper(value)]
		if !ok {
			return errors.New("cannot verify saved SQL_MODE during packet preflight")
		}
	} else {
		*mode = quoteRules(strings.Trim(value, "'"))
	}
	return nil
}
