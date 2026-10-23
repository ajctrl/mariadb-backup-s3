package tests

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHistoryPreflightRejectsBeforeAnyDump(t *testing.T) {
	for _, mode := range []string{"per-database", "combined"} {
		for _, scenario := range []string{"unsupported", "ignored", "data-only-excluded", "query-failure", "malformed"} {
			t.Run(mode+"/"+scenario, func(t *testing.T) {
				f := newFixture(t)
				f.env["MARIADB_DATABASES"], f.env["BACKUP_MODE"] = "app,billing", mode
				f.env["BACKUP_KEEP_DAYS"] = "1"
				f.env["FAKE_HISTORY_FILE"] = filepath.Join(f.dir, "history.hex")
				data := hex.EncodeToString([]byte("billing")) + "\t" + hex.EncodeToString([]byte("history")) + "\n"
				want := "unsupported transaction-precise system-versioned tables: \"billing\".\"history\""
				switch scenario {
				case "ignored":
					f.env["MARIADB_DUMP_EXTRA_OPTS"] = "--ignore-table=billing.history"
					// Exclusions are applied in SQL; real collation behavior is
					// covered by the Docker integration tests.
					data = ""
				case "data-only-excluded":
					f.env["MARIADB_DUMP_EXTRA_OPTS"] = "--ignore-table-data=billing.history"
				case "query-failure":
					f.env["FAKE_HISTORY_FAILURE"] = "true"
					want = "could not check system-versioned table support"
				case "malformed":
					data, want = "not hex\n", "invalid system-versioned table metadata result"
				}
				if err := os.WriteFile(f.env["FAKE_HISTORY_FILE"], []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
				out := f.run(scenario == "ignored", "backup")
				if scenario == "ignored" {
					assertEqual(t, f.databaseCalls("mariadb-dump"), []string{"app", "billing"})
				} else {
					if !strings.Contains(out, want) {
						t.Fatalf("missing preflight diagnostic %q: %s", want, out)
					}
					assertEqual(t, len(f.processes("mariadb-dump")), 0)
					assertEqual(t, len(f.s3.calls("")), 0)
				}
			})
		}
	}
}

func TestHistoryPreflightUsesOnlySelectedSchemasAndHexNames(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_BACKUP_ALL"], f.env["MARIADB_DATABASES_EXCLUDE"] = "true", "excluded"
	f.databases([]string{"a'b\\日本語", "excluded"})
	f.run(true, "backup")
	calls := f.processes("mariadb")
	assertEqual(t, len(calls), 2)
	data, err := os.ReadFile(f.env["FAKE_HISTORY_QUERY"])
	if err != nil {
		t.Fatal(err)
	}
	query := string(data)
	if !strings.Contains(query, "TABLE_SCHEMA = _utf8mb4 0x"+hex.EncodeToString([]byte("a'b\\日本語"))) || strings.Contains(query, "6578636c75646564") || strings.Contains(query, "a'b") {
		t.Fatalf("unsafe or incorrectly scoped metadata query: %s", query)
	}
}

func TestBackupTimeoutKillsProcessesAndReleasesLock(t *testing.T) {
	for _, command := range []string{"mariadb", "mariadb-dump", "gpg"} {
		t.Run(command, func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_DATABASE"], f.env["BACKUP_TIMEOUT"] = "app", "600ms"
			if command == "gpg" {
				f.env["PASSPHRASE"] = "test"
			}
			entered, release := filepath.Join(f.dir, "entered"), filepath.Join(f.dir, "release")
			pidPath := filepath.Join(f.dir, "child.pid")
			f.env["FAKE_PAUSE_COMMAND"], f.env["FAKE_PAUSE_ENTERED"], f.env["FAKE_PAUSE_RELEASE"], f.env["FAKE_CHILD_PID"] = command, entered, release, pidPath
			p := f.background("backup")
			waitForFile(t, entered, p)
			if err := p.wait(t); err == nil {
				t.Fatal("timed out backup returned success")
			}
			out, _ := os.ReadFile(p.output)
			if !strings.Contains(string(out), "backup exceeded BACKUP_TIMEOUT=600ms") {
				t.Fatalf("missing timeout diagnostic: %s", out)
			}
			pidData, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
				t.Fatalf("process %d survived timeout: %v", pid, err)
			}
			for _, call := range f.processes("mariadb") {
				path := strings.TrimPrefix(call.Args[0], "--defaults-file=")
				if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
					t.Fatalf("credentials directory survived timeout: %v", err)
				}
			}
			assertEqual(t, len(f.s3.calls("PUT")), 0)
			f.assertClean()
			next := newFixture(t)
			next.env["MARIADB_DATABASE"] = "app"
			next.run(true, "backup")
		})
	}
}

func TestBackupTimeoutCancelsS3Request(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["BACKUP_TIMEOUT"] = "app", "600ms"
	f.s3.pauseMethod, f.s3.pauseEntered, f.s3.pauseRelease = "PUT", make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(f.s3.pauseRelease) })
	p := f.background("backup")
	select {
	case <-f.s3.pauseEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	if err := p.wait(t); err == nil {
		t.Fatal("stalled upload returned success")
	}
	out, _ := os.ReadFile(p.output)
	if !strings.Contains(string(out), "backup exceeded BACKUP_TIMEOUT=600ms") {
		t.Fatalf("missing upload timeout diagnostic: %s", out)
	}
	next := newFixture(t)
	next.env["MARIADB_DATABASE"] = "app"
	next.run(true, "backup")
}

func TestScheduledBackupRecoversAfterTimeout(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["SCHEDULE"], f.env["BACKUP_TIMEOUT"] = "app", "@every 1s", "400ms"
	entered, release := filepath.Join(f.dir, "entered"), filepath.Join(f.dir, "release")
	f.env["FAKE_PAUSE_COMMAND"], f.env["FAKE_PAUSE_ENTERED"], f.env["FAKE_PAUSE_RELEASE"] = "mariadb-dump", entered, release
	p := f.background("run")
	waitForFile(t, entered, p)
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := os.ReadFile(p.output)
		if strings.Contains(string(out), "Scheduled backup failed: backup exceeded BACKUP_TIMEOUT=400ms") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scheduler did not report timeout: %s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		out, _ := os.ReadFile(p.output)
		if strings.Contains(string(out), "Backup summary: 1 succeeded, 0 failed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("next scheduled backup did not recover: %s", out)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = p.wait(t)
	if len(f.s3.calls("PUT")) == 0 || len(f.processes("mariadb-dump")) < 2 {
		t.Fatal("next scheduled run did not upload")
	}
	f.assertClean()
}

func TestHistoryPreflightLargeDatabaseSelection(t *testing.T) {
	for _, mode := range []string{"per-database", "combined"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			names := make([]string, 1000)
			for i := range names {
				names[i] = "database_" + strconv.Itoa(i)
			}
			f.env["MARIADB_DATABASES"] = strings.Join(names, ",")
			f.env["BACKUP_MODE"] = mode
			// Reject a table in the last DB: every DB must be checked before dumping.
			f.env["FAKE_HISTORY_FILE"] = filepath.Join(f.dir, "last.hex")
			if err := os.WriteFile(f.env["FAKE_HISTORY_FILE"], []byte(hex.EncodeToString([]byte(names[999]))+"\t686973746f7279\n"), 0600); err != nil {
				t.Fatal(err)
			}
			out := f.run(false, "backup")
			if !strings.Contains(out, "unsupported transaction-precise") {
				t.Fatal(out)
			}
			query, err := os.ReadFile(f.env["FAKE_HISTORY_QUERY"])
			if err != nil {
				t.Fatal(err)
			}
			if len(query) < 131072 || strings.Count(string(query), "SELECT DISTINCT") != 1000 {
				t.Fatal("large preflight was truncated")
			}
			assertEqual(t, len(f.processes("mariadb-dump")), 0)
			assertEqual(t, len(f.s3.calls("")), 0)
		})
	}
}
