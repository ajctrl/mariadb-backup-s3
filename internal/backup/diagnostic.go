package backup

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
)

// Only retain the bounded prefix of the first MariaDB error line. Server
// messages, including continuation lines, can contain arbitrary row values.
var sqlErrorHeader = regexp.MustCompile(`^(ERROR [0-9]{1,5} \([A-Z0-9]{5}\))(?: at line ([0-9]+))?:`)

type restoreDiagnostic struct {
	prefix   []byte
	skipLine bool
	header   string
}

func (d *restoreDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 && d.header == "" {
		line, rest, ended := bytes.Cut(p, []byte{'\n'})
		if !d.skipLine {
			d.prefix = append(d.prefix, line[:min(len(line), 96-len(d.prefix))]...)
			if match := sqlErrorHeader.Find(d.prefix); match != nil {
				d.header = string(match)
				break
			}
			d.skipLine = len(d.prefix) == 96
		}
		if !ended {
			break
		}
		d.prefix = d.prefix[:0]
		d.skipLine = false
		p = rest
	}
	return n, nil
}

func (d *restoreDiagnostic) err(preambleLines int) error {
	if d.header == "" {
		return nil
	}
	parts := sqlErrorHeader.FindStringSubmatch(d.header)
	if line, err := strconv.Atoi(parts[2]); err == nil && line > preambleLines {
		return fmt.Errorf("%s at line %d", parts[1], line-preambleLines)
	}
	return fmt.Errorf("%s", parts[1])
}
