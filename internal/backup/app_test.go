package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ajctrl/mariadb-backup-s3/internal/storage"
)

type recordingStore struct {
	objects    []storage.Object
	deleted    []string
	batchSizes []int
	prefix     string
	listErr    error
	serverTime time.Time
}

func (s *recordingStore) ManagesRetention() bool                                     { return true }
func (s *recordingStore) VersioningEnabled(context.Context) (bool, error)            { return true, nil }
func (s *recordingStore) Upload(context.Context, string, io.ReadCloser, bool) error  { return nil }
func (s *recordingStore) Download(context.Context, string, string) (*os.File, error) { return nil, nil }
func (s *recordingStore) List(_ context.Context, prefix string, visit func(storage.Object, time.Time) error) error {
	s.prefix = prefix
	for _, obj := range s.objects {
		if err := visit(obj, s.serverTime); err != nil {
			return err
		}
	}
	return s.listErr
}
func (s *recordingStore) Delete(_ context.Context, objects []storage.Object) error {
	s.batchSizes = append(s.batchSizes, len(objects))
	for _, obj := range objects {
		s.deleted = append(s.deleted, obj.Key)
	}
	return nil
}

func TestRetentionBoundaryAndExactMatching(t *testing.T) {
	cutoff := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	expired := []storage.Object{
		{Key: "backup/app/2026-09-05T11:59:59.sql", ETag: "old", LastModified: cutoff.Add(-time.Second)},
		{Key: "backup/app/2026-09-05T11:59:59.123456789.sql.gpg", ETag: "old", LastModified: cutoff.Add(-time.Nanosecond)},
	}
	objects := append([]storage.Object{}, expired...)
	savedKey := "backup/app/2026-09-05T11:10:00.sql"
	objects = append(objects,
		storage.Object{Key: savedKey, LastModified: cutoff.Add(-24 * time.Hour)}, storage.Object{Key: "backup/app/2026-09-05T12:00:00.sql", LastModified: cutoff},
		storage.Object{Key: "backup/app/2026-09-05T12:00:01.sql.gpg", LastModified: cutoff.Add(time.Nanosecond)},
	)
	for _, key := range []string{
		"backup/app_latest.sql", "backup/app_latest.sql.gpg", "backup/app/extra_2000-01-01T00:00:00.sql",
		"backup/app/2000-01-01T00%3A00%3A00/latest.sql", "backup/other_2000-01-01T00:00:00.sql",
		"backup/app/notes.sql", "backup/app/2000-01-01T00:00:00.sql.extra", "backup/app/2000-01-01T00:00:00.sql\n",
		"backup/app/2025-02-29T00:00:00.sql", "backup/app/2026-09-05T23:59:60.000000000.sql.gpg",
	} {
		objects = append(objects, storage.Object{Key: key, LastModified: cutoff.Add(-time.Hour)})
	}
	for _, mode := range []string{"timestamp", "fixed"} {
		s := &recordingStore{objects: objects, serverTime: cutoff.AddDate(0, 0, 1)}
		a := New(Config{Prefix: "backup", FilenameMode: mode}, s, io.Discard, io.Discard)
		if err := a.removeOldBackups(context.Background(), "app", savedKey, 1); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s.deleted, []string{expired[0].Key, expired[1].Key}) || s.prefix != "backup/app/" {
			t.Errorf("mode %s: deleted %v, prefix %q", mode, s.deleted, s.prefix)
		}
	}
}

func TestRetentionNeverDeletesAfterIncompleteListing(t *testing.T) {
	for _, s := range []*recordingStore{
		{objects: []storage.Object{{Key: "backup/app/2000-01-01T00:00:00.sql"}}},
		{listErr: errors.New("second page failed")},
	} {
		a := New(Config{Prefix: "backup"}, s, io.Discard, io.Discard)
		if err := a.removeOldBackups(context.Background(), "app", "saved", 1); err == nil || len(s.deleted) != 0 {
			t.Errorf("unsafe incomplete listing: err=%v deleted=%v", err, s.deleted)
		}
	}
}

func TestBackupLockAndCanceledRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "backup.lock")
	lock, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireLock(path); err == nil {
		second.Close()
		t.Fatal("overlapping lock accepted")
	}
	lock.Close()
	second, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	a := New(Config{}, &recordingStore{}, io.Discard, io.Discard)
	a.lockPath = path
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Backup(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled backup = %v", err)
	}
}

func TestRetentionBatchesOnlyAfterLastPage(t *testing.T) {
	now := time.Now().UTC()
	s := &recordingStore{serverTime: now}
	for i := 0; i < 1001; i++ {
		s.objects = append(s.objects, storage.Object{
			Key:  fmt.Sprintf("backup/app/2000-01-01T00:00:00.%09d.sql", i),
			ETag: "old", LastModified: now.AddDate(0, 0, -2),
		})
	}
	a := New(Config{Prefix: "backup"}, s, io.Discard, io.Discard)
	s.listErr = errors.New("second page failed")
	if err := a.removeOldBackups(t.Context(), "app", "saved", 1); err == nil || len(s.deleted) != 0 {
		t.Fatalf("incomplete listing: err=%v deletes=%d", err, len(s.deleted))
	}
	s.listErr = nil
	if err := a.removeOldBackups(t.Context(), "app", "saved", 1); err != nil || !reflect.DeepEqual(s.batchSizes, []int{1000, 1}) {
		t.Fatalf("complete listing: err=%v batches=%v", err, s.batchSizes)
	}
}
