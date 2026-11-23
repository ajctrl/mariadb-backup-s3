// Package tempfile provides private staging files for backup jobs on Linux.
package tempfile

import "os"

// Open unlinks the file before the caller writes so a killed job leaves no data at a pathname.
func Open(dir, pattern string) (*os.File, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
