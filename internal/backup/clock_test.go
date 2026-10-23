package backup

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ajctrl/mariadb-backup-s3/internal/storage"
)

type guardedStore struct {
	recordingStore
	beforePublish func()
	published     int
	uploads       int
	requireAbsent bool
}

func (s *guardedStore) Upload(_ context.Context, _ string, body io.ReadCloser, options storage.UploadOptions) error {
	defer body.Close()
	s.uploads++
	s.requireAbsent = options.RequireAbsent
	if _, err := io.Copy(io.Discard, body); err != nil {
		return err
	}
	if s.beforePublish != nil {
		s.beforePublish()
	}
	if options.BeforePublish == nil {
		return errors.New("missing publication guard")
	}
	if err := options.BeforePublish(); err != nil {
		return err
	}
	s.published++
	return nil
}

func TestBackupClockRollbackStopsPublicationAndRetention(t *testing.T) {
	for _, filenameMode := range []string{"timestamp", "fixed"} {
		for _, backupMode := range []string{"per-database", "combined"} {
			for _, stage := range []string{"before dump", "before publication", "during publication", "during retention listing", "normal", "equal"} {
				t.Run(filenameMode+"/"+backupMode+"/"+stage, func(t *testing.T) {
					bin := t.TempDir()
					for name, script := range map[string]string{
						"mariadb":      "#!/bin/sh\nexit 0\n",
						"mariadb-dump": "#!/bin/sh\nprintf 'backup SQL\\n'\n",
					} {
						if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
							t.Fatal(err)
						}
					}
					t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
					base := time.Date(2026, 10, 22, 12, 0, 0, 123456789, time.UTC)
					s := &guardedStore{}
					c := Config{Databases: "app,analytics", BackupAll: "false", FilenameMode: filenameMode, BackupMode: backupMode, KeepDays: "7", Prefix: "backup"}
					oldKey := c.timestampPrefix("app") + "2000-01-01T00:00:00.000000000Z-00000000000000000000000000000000.sql"
					s.objects = []storage.Object{{Key: oldKey, LastModified: base.Add(-30 * 24 * time.Hour)}}
					a := New(c, s, io.Discard, io.Discard)
					a.lockPath = filepath.Join(t.TempDir(), "backup.lock")
					calls := 0
					rollbackAt := map[string]int{"before dump": 2, "before publication": 3, "during publication": 4, "during retention listing": 5}[stage]
					a.now = func() time.Time {
						calls++
						if rollbackAt > 0 && calls >= rollbackAt {
							return base.Add(-time.Nanosecond)
						}
						if stage == "equal" {
							return base
						}
						return base.Add(time.Duration(calls) * time.Second)
					}
					err := a.Backup(context.Background())
					if rollbackAt > 0 {
						if !errors.Is(err, errClockRollback) {
							t.Fatalf("expected rollback error, got %v", err)
						}
						wantPublished := 0
						if rollbackAt >= 4 {
							wantPublished = 1
						}
						if s.published != wantPublished || len(s.deleted) != 0 || s.uploads > 1 {
							t.Fatalf("published=%d deleted=%v uploads=%d", s.published, s.deleted, s.uploads)
						}
						if rollbackAt <= 4 && s.prefix != "" {
							t.Fatal("retention ran after failed backup")
						}
					} else {
						if err != nil {
							t.Fatal(err)
						}
						want := 2
						if backupMode == "combined" {
							want = 1
						}
						if s.published != want || len(s.deleted) != 1 {
							t.Fatalf("published=%d deleted=%v", s.published, s.deleted)
						}
					}
					if s.uploads > 0 && s.requireAbsent != (filenameMode == "timestamp") {
						t.Fatal("wrong overwrite policy")
					}
				})
			}
		}
	}
}

func TestBackupClockRollbackRemainsFailedAfterRecovery(t *testing.T) {
	start := time.Now()
	now := start.Add(-time.Nanosecond)
	c := &backupClock{now: func() time.Time { return now }, latest: start}
	if err := c.check(); !errors.Is(err, errClockRollback) {
		t.Fatal(err)
	}
	now = start.Add(time.Hour)
	if err := c.check(); !errors.Is(err, errClockRollback) {
		t.Fatal("recovered clock cleared failure")
	}
}

func TestRetentionStopsBeforeNextDeleteOnClockRollback(t *testing.T) {
	start := time.Now().UTC()
	s := &recordingStore{}
	for _, id := range []string{strings.Repeat("0", 32), strings.Repeat("1", 32)} {
		s.objects = append(s.objects, storage.Object{Key: "backup/app/2000-01-01T00:00:00.000000000Z-" + id + ".sql", LastModified: start.Add(-time.Hour)})
	}
	a := New(Config{Prefix: "backup"}, s, io.Discard, io.Discard)
	c := &backupClock{latest: start, now: func() time.Time {
		if len(s.deleted) > 0 {
			return start.Add(-time.Second)
		}
		return start
	}}
	if err := a.removeOldBackups(context.Background(), "app", start, c.check); !errors.Is(err, errClockRollback) {
		t.Fatal(err)
	}
	if len(s.deleted) != 1 {
		t.Fatalf("deleted %v", s.deleted)
	}
}
