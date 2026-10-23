package backup

import (
	"errors"
	"fmt"
	"time"
)

var errClockRollback = errors.New("wall clock moved backwards")

// A run shares one guard across database backups and retention. A detected
// rollback is sticky, even if the clock catches up before the next check.
// Calls are sequential: publication completes before retention or the next DB.
type backupClock struct {
	now    func() time.Time
	latest time.Time
	err    error
}

func (c *backupClock) read() (time.Time, error) {
	// UTC strips Go's monotonic reading: we must compare wall-clock values.
	now := c.now().UTC()
	if c.err == nil && now.Before(c.latest) {
		c.err = fmt.Errorf("%w: %s precedes %s; stopping backup and retention", errClockRollback,
			now.Format(time.RFC3339Nano), c.latest.Format(time.RFC3339Nano))
	}
	if c.err == nil {
		c.latest = now
	}
	return now, c.err
}

func (c *backupClock) check() error {
	_, err := c.read()
	return err
}
