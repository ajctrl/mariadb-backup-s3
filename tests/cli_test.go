// Package tests exercises the built CLI against a local S3 HTTP endpoint and
// fake MariaDB/GPG executables. No Python, AWS CLI, or external service is needed.
package tests

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var cliBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mariadb-backup-s3-cli-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cliBinary = filepath.Join(dir, "mariadb-backup-s3")
	args := []string{"build"}
	if os.Getenv("CLI_TEST_RACE") == "1" {
		args = append(args, "-race")
	}
	build := exec.Command("go", append(args, "-o", cliBinary, "./cmd/mariadb-backup-s3")...)
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build CLI: %v\n%s", err, output)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

const fakeTool = `#!/bin/sh
set -eu
command=${0##*/}
line=$command
for arg do
 case "$arg" in --xml) [ "${FAKE_ROLE_LOOKUP_FAILURE:-}" != true ] || exit 1; exit 0;; esac
 case "$arg" in --unbuffered)
  [ -z "${FAKE_LOCK_PID:-}" ] || printf '%s\n' "$$" > "$FAKE_LOCK_PID"
  while IFS= read -r query; do
    case "$query" in
      *"SELECT GET_LOCK("* ) printf '%s\t0\t%s\t\n' "${FAKE_LOCK_RESULT:-1}" "${FAKE_MAX_PACKET:-16777216}";;
      "SELECT IS_USED_LOCK("* ) printf '%s\n' "${FAKE_LOCK_RESULT:-1}";;
      *) first_sql=$query; break;;
    esac
  done
  [ "${first_sql+x}" = x ] || exit 0
  break;; esac
 case "$arg" in --raw) metadata_candidate=true;; esac
done
case "$*:${metadata_candidate:-}" in *--execute=*) ;; *:true) cat >/dev/null; printf '%s\n' metadata >> "$FAKE_LOG"; printf '%s' "${FAKE_TABLE_METADATA:-}"; exit 0;; esac
for arg do line="$line	$arg"; done
printf '%s\n' "$line" >> "$FAKE_LOG"
database=''
databases=''
output=''
previous=''
decrypt=false
discovery=false
positional=false
passphrase_fd=''
for arg do
  case "$previous" in --output|-o) output=$arg;; esac
  case "$previous" in --passphrase-fd) passphrase_fd=$arg;; esac
  [ "$arg" != --passphrase ] || exit 11
  if [ "$positional" = true ]; then
    [ -n "$database" ] || database=$arg
    databases="$databases [$arg]"
  else
    case "$arg" in
      --) positional=true;;
      --database=*) database=${arg#--database=};;
      --execute=*) discovery=true;;
    esac
  fi
  [ "$arg" != --decrypt ] || decrypt=true
  previous=$arg
done
[ -z "${PASSPHRASE:-}" ] || exit 12
if [ "$command" != gpg ]; then
  [ -z "${MARIADB_PASSWORD:-}" ] || exit 9
  [ -z "${MYSQL_PWD:-}" ] || exit 10
  case "$1" in --defaults-file=*) defaults=${1#--defaults-file=};; *) exit 6;; esac
  [ "$(stat -Lc %a "$defaults")" = 600 ] || exit 7
  case "$(readlink "$defaults")" in *" (deleted)") ;; *) exit 8;; esac
  [ -z "${FAKE_DEFAULTS_SNAPSHOT:-}" ] || cp "$defaults" "$FAKE_DEFAULTS_SNAPSHOT"
else
  case "$passphrase_fd" in 3|4) ;; *) exit 13;; esac
  [ "$(stat -Lc %a "/proc/self/fd/$passphrase_fd")" = 600 ] || exit 14
  passphrase=$(cat "/proc/self/fd/$passphrase_fd")
  [ -n "$passphrase" ] || exit 15
  [ -z "${FAKE_GPG_PASSPHRASE_SNAPSHOT:-}" ] || printf '%s' "$passphrase" > "$FAKE_GPG_PASSPHRASE_SNAPSHOT"
fi
if [ "$command" = "${FAKE_PAUSE_COMMAND:-}" ]; then
  [ -z "${FAKE_CHILD_PID:-}" ] || printf '%s\n' "$$" > "$FAKE_CHILD_PID"
  touch "$FAKE_PAUSE_ENTERED"
  while [ ! -f "$FAKE_PAUSE_RELEASE" ]; do sleep 0.02; done
fi
case "$command" in
  mariadb)
    if [ "$discovery" = true ]; then
      [ "${FAKE_DISCOVERY_FAILURE:-}" != true ] || exit 1
      cat "$FAKE_DATABASES_FILE"
    else
      payload=$(printf '%s\n' "$first_sql"; cat)
      case "$payload" in '-- SQL'*) ;; *) exit 3;; esac
      [ -z "${FAKE_RESTORE_INPUT:-}" ] || printf '%s\n' "$payload" > "$FAKE_RESTORE_INPUT"
      [ "${FAKE_RESTORE_FAILURE:-}" != true ] || exit 1
    fi
    ;;
  mariadb-dump)
    if [ -n "${FAKE_EMPTY_DUMP:-}" ] && [ "$database" = "$FAKE_EMPTY_DUMP" ]; then
      exit 0
    fi
    if [ "$database" = "${FAKE_DUMP_FAILURE:-}" ]; then
      printf 'partial dump\n'
      exit 1
    fi
    printf '%s\n' "-- SQL$databases"
    ;;
  gpg)
    if [ "$decrypt" = true ]; then
      payload=$(cat)
      if [ -n "${FAKE_GPG_FAILURE:-}" ]; then
        case "$payload" in *"$FAKE_GPG_FAILURE"*) exit 1;; esac
      fi
      printf '%s\n' "$payload"
    else
      payload=$(cat)
      if [ -n "${FAKE_GPG_FAILURE:-}" ]; then
        case "$payload" in *"$FAKE_GPG_FAILURE"*) exit 1;; esac
      fi
      [ "$output" = - ] || exit 5
      printf '%s\n' "$payload"
    fi
    ;;
  *) exit 4;;
esac
`

type object struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type request struct {
	Method  string
	Key     string
	Query   url.Values
	Payload string
}

type fakeS3 struct {
	mu            sync.Mutex
	ready         chan struct{}
	readyOnce     sync.Once
	requests      []request
	objects       []object
	versioning    string
	fail          string
	uploadFailure string
	pauseMethod   string
	pauseEntered  chan struct{}
	pauseRelease  chan struct{}
	pauseOnce     sync.Once
	pageSize      int
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Fixture settings are frozen before its first CLI starts. Synchronize
	// with that point explicitly: process startup is not a Go memory barrier.
	<-s.ready
	query := r.URL.Query()
	key := strings.TrimPrefix(r.URL.Path, "/test-bucket/")
	if r.URL.Path == "/test-bucket" || r.URL.Path == "/test-bucket/" {
		key = ""
	}
	body, _ := io.ReadAll(r.Body)
	if query.Has("delete") {
		var batch struct {
			Objects []struct{ Key, ETag string } `xml:"Object"`
		}
		if err := xml.Unmarshal(body, &batch); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		for _, obj := range batch.Objects {
			if obj.ETag != "test-etag" {
				http.Error(w, "missing specific ETag", 400)
				return
			}
			s.mu.Lock()
			s.requests = append(s.requests, request{"DELETE", obj.Key, query, ""})
			s.mu.Unlock()
		}
		if s.pauseMethod == "DELETE" {
			s.pauseOnce.Do(func() { close(s.pauseEntered) })
			select {
			case <-s.pauseRelease:
			case <-r.Context().Done():
				return
			}
		}
		if s.fail == "DELETE" {
			io.WriteString(w, "<DeleteResult><Error><Code>AccessDenied</Code></Error></DeleteResult>")
			return
		}
		result := struct {
			XMLName xml.Name               `xml:"DeleteResult"`
			Deleted []struct{ Key string } `xml:"Deleted"`
		}{}
		for _, obj := range batch.Objects {
			result.Deleted = append(result.Deleted, struct{ Key string }{obj.Key})
		}
		xml.NewEncoder(w).Encode(result)
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, request{r.Method, key, query, string(body)})
	s.mu.Unlock()
	if r.Method == s.pauseMethod {
		s.pauseOnce.Do(func() { close(s.pauseEntered) })
		select {
		case <-s.pauseRelease:
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/xml")
	action := r.Method
	if query.Has("versioning") {
		action = "versioning"
	} else if query.Get("list-type") == "2" {
		action = "list"
	}
	if action == s.fail || (s.fail == "part" && query.Has("partNumber")) || (action == "PUT" && s.uploadFailure != "" && strings.Contains(key, s.uploadFailure)) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>Test failure</Message></Error>`)
		return
	}
	switch action {
	case "POST":
		if query.Has("uploads") {
			io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>stream</UploadId></InitiateMultipartUploadResult>`)
		} else if query.Has("uploadId") {
			io.WriteString(w, `<CompleteMultipartUploadResult><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	case "versioning":
		xml.NewEncoder(w).Encode(struct {
			XMLName xml.Name `xml:"VersioningConfiguration"`
			Status  string   `xml:"Status,omitempty"`
		}{Status: s.versioning})
	case "list":
		var matching []object
		for _, obj := range s.objects {
			if strings.HasPrefix(obj.Key, query.Get("prefix")) {
				obj.ETag = `"test-etag"`
				matching = append(matching, obj)
			}
		}
		start, _ := strconv.Atoi(query.Get("continuation-token"))
		end := len(matching)
		if s.pageSize > 0 && end > start+s.pageSize {
			end = start + s.pageSize
		}
		next := ""
		if end < len(matching) {
			next = strconv.Itoa(end)
		}
		xml.NewEncoder(w).Encode(struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			Name                  string   `xml:"Name"`
			IsTruncated           bool     `xml:"IsTruncated"`
			NextContinuationToken string   `xml:"NextContinuationToken,omitempty"`
			Contents              []object `xml:"Contents"`
		}{Name: "test-bucket", IsTruncated: next != "", NextContinuationToken: next, Contents: matching[start:end]})
	case "PUT":
		w.Header().Set("ETag", `"test-etag"`)
		w.WriteHeader(http.StatusOK)
	case "GET":
		if version := query.Get("versionId"); version != "" {
			w.Header().Set("x-amz-version-id", version)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		io.WriteString(w, "-- SQL restored\n")
	case "DELETE":
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "Unexpected S3 request: %s %s", r.Method, r.URL)
	}
}

func (s *fakeS3) calls(action string) []request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []request
	for _, call := range s.requests {
		kind := call.Method
		if call.Query.Has("versioning") {
			kind = "versioning"
		} else if call.Query.Get("list-type") == "2" {
			kind = "list"
		}
		if action == "" || action == kind {
			result = append(result, call)
		}
	}
	return result
}

type fixture struct {
	t       *testing.T
	dir     string
	runtime string
	log     string
	env     map[string]string
	s3      *fakeS3
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	bin, runtime := filepath.Join(dir, "bin"), filepath.Join(dir, "runtime")
	for _, path := range []string{bin, runtime} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []string{"mariadb", "mariadb-dump", "gpg"} {
		if err := os.WriteFile(filepath.Join(bin, command), []byte(fakeTool), 0700); err != nil {
			t.Fatal(err)
		}
	}
	s3 := &fakeS3{versioning: "Enabled", ready: make(chan struct{})}
	server := httptest.NewServer(s3)
	t.Cleanup(server.Close)
	f := &fixture{t: t, dir: dir, runtime: runtime, log: filepath.Join(dir, "calls.log"), s3: s3}
	f.env = map[string]string{
		"PATH":   bin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMPDIR": runtime, "FAKE_LOG": f.log, "FAKE_DATABASES_FILE": filepath.Join(dir, "databases.hex"),
		"S3_ALLOW_HTTP": "true", "S3_BUCKET": "test-bucket", "S3_PREFIX": "backup", "S3_REGION": "us-west-1", "S3_ENDPOINT": server.URL,
		"S3_ACCESS_KEY_ID": "test-key", "S3_SECRET_ACCESS_KEY": "test-secret", "AWS_EC2_METADATA_DISABLED": "true",
		"MARIADB_HOST": "mariadb", "MARIADB_USER": "backup", "MARIADB_PASSWORD": "test",
	}
	f.databases([]string{"app", "analytics"})
	return f
}

func (f *fixture) databases(names []string) {
	f.t.Helper()
	var data strings.Builder
	for _, name := range names {
		data.WriteString(hex.EncodeToString([]byte(name)))
		data.WriteByte('\n')
	}
	if err := os.WriteFile(f.env["FAKE_DATABASES_FILE"], []byte(data.String()), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) command(args ...string) *exec.Cmd {
	f.t.Helper()
	f.s3.readyOnce.Do(func() { close(f.s3.ready) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	f.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, cliBinary, args...)
	cmd.Dir = f.dir
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "MARIADB_") || strings.HasPrefix(key, "MYSQL_") || strings.HasPrefix(key, "S3_") || strings.HasPrefix(key, "AWS_") || strings.HasPrefix(key, "BACKUP_") || key == "PASSPHRASE" || key == "SCHEDULE" || key == "TMPDIR" || key == "PATH" {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	for key, value := range f.env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	return cmd
}

func (f *fixture) run(wantSuccess bool, args ...string) string {
	f.t.Helper()
	output, err := f.command(args...).CombinedOutput()
	if (err == nil) != wantSuccess {
		f.t.Fatalf("CLI %v: error = %v, want success %t\n%s", args, err, wantSuccess, output)
	}
	f.assertClean()
	return string(output)
}

func (f *fixture) assertClean() {
	f.t.Helper()
	files, err := os.ReadDir(f.runtime)
	if err != nil {
		f.t.Fatal(err)
	}
	if len(files) != 0 {
		f.t.Fatalf("temporary files leaked: %v", files)
	}
}

type processCall struct {
	Command string
	Args    []string
}

func (f *fixture) processes(command string) []processCall {
	f.t.Helper()
	data, err := os.ReadFile(f.log)
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	var calls []processCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if command == "" || fields[0] == command {
			calls = append(calls, processCall{fields[0], fields[1:]})
		}
	}
	return calls
}

func (f *fixture) databaseCalls(command string) []string {
	f.t.Helper()
	var databases []string
	for _, call := range f.processes(command) {
		found := false
		for i, arg := range call.Args {
			if command == "mariadb-dump" && arg == "--" && i+1 < len(call.Args) {
				databases = append(databases, call.Args[i+1:]...)
				found = true
				break
			}
			if command == "mariadb" && strings.HasPrefix(arg, "--database=") {
				databases = append(databases, strings.TrimPrefix(arg, "--database="))
				found = true
				break
			}
		}
		if !found {
			f.t.Fatalf("missing database argument: %v", call.Args)
		}
	}
	return databases
}

func keys(calls []request) []string {
	result := make([]string, 0, len(calls))
	for _, call := range calls {
		result = append(result, call.Key)
	}
	return result
}

func assertEqual[T any](t *testing.T, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

func TestBackupSingleDatabaseAndLiteralNames(t *testing.T) {
	for _, name := range []string{"app", "with space", "ALL", "with,comma", "a'b", "a/b", "host=other", "$(oops)*", "--all-databases"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_DATABASE"] = name
			f.run(true, "backup")
			assertEqual(t, f.databaseCalls("mariadb-dump"), []string{name})
			calls := f.s3.calls("PUT")
			assertEqual(t, len(calls), 1)
			encoded := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
			pattern := "^" + regexp.QuoteMeta("backup/"+encoded+"/") + `[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{9}\.sql$`
			if !regexp.MustCompile(pattern).MatchString(calls[0].Key) {
				t.Fatalf("unexpected timestamp key: %q", calls[0].Key)
			}
			if !strings.Contains(calls[0].Payload, "-- SQL") {
				t.Fatalf("upload does not contain the completed dump: %q", calls[0].Payload)
			}
			assertEqual(t, len(f.s3.calls("list")), 0)
		})
	}
}

func TestDatabaseListOrderDeduplicationAndPrecedence(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["MARIADB_DATABASES"] = "ignored", " billing, ALL,app,billing "
	f.run(true, "backup")
	assertEqual(t, f.databaseCalls("mariadb-dump"), []string{"billing", "ALL", "app"})
	assertEqual(t, len(f.processes("mariadb")), 0)
}

func TestDefaultEntrypointAndRunWithoutSchedule(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_DATABASE"] = "app"
			f.env["MARIADB_DUMP_EXTRA_OPTS"] = "--skip-comments  --ignore-table=app.tmp"
			f.run(true, args...)
			assertEqual(t, f.databaseCalls("mariadb-dump"), []string{"app"})
			calls := f.processes("mariadb-dump")
			assertEqual(t, calls[0].Args[len(calls[0].Args)-4:], []string{"--skip-comments", "--ignore-table=app.tmp", "--", "app"})
		})
	}
}

func TestUnsetRetentionDoesNotListOrDeleteExistingObjects(t *testing.T) {
	for _, mode := range []string{"timestamp", "fixed"} {
		for _, explicitlyEmpty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%t", mode, explicitlyEmpty), func(t *testing.T) {
				f := newFixture(t)
				f.env["MARIADB_DATABASES"], f.env["BACKUP_FILENAME_MODE"] = "app,billing", mode
				if explicitlyEmpty {
					f.env["BACKUP_KEEP_DAYS"] = ""
				}
				f.s3.fail = "list"
				f.s3.objects = []object{{Key: "backup/app/2000-01-01T00:00:00.sql", LastModified: "2000-01-01T00:00:00Z"}}
				f.run(true, "backup")
				assertEqual(t, len(f.s3.calls("PUT")), 2)
				assertEqual(t, len(f.s3.calls("list")), 0)
				assertEqual(t, len(f.s3.calls("DELETE")), 0)
			})
		}
	}
}

func TestAllDatabaseDiscoveryAndExactExclusions(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_BACKUP_ALL"] = "true"
	f.env["MARIADB_DATABASES_EXCLUDE"] = "scratch"
	f.databases([]string{"ALL", "app", "analytics", "scratch", "scratchpad", "app"})
	f.run(true, "backup")
	assertEqual(t, f.databaseCalls("mariadb-dump"), []string{"ALL", "app", "analytics", "scratchpad"})
	assertEqual(t, len(f.processes("mariadb")), 1)
	args := strings.Join(f.processes("mariadb")[0].Args, " ")
	for _, required := range []string{"HEX(SCHEMA_NAME)", "information_schema.SCHEMATA", "ORDER BY SCHEMA_NAME", "'mysql'", "'sys'", "'information_schema'", "'performance_schema'", "--batch", "--raw", "--skip-column-names"} {
		if !strings.Contains(args, required) {
			t.Errorf("database discovery missing %q: %s", required, args)
		}
	}
}

func TestInvalidSelectionAndRetentionNeverAccessDatabaseOrS3(t *testing.T) {
	cases := []map[string]string{
		{}, {"MARIADB_BACKUP_ALL": "ALL"},
		{"MARIADB_BACKUP_ALL": "true", "MARIADB_DATABASE": "app"},
		{"MARIADB_BACKUP_ALL": "true", "MARIADB_DATABASES": "app"},
		{"MARIADB_DATABASES": "app,,billing"}, {"MARIADB_DATABASES": "app,"},
		{"MARIADB_DATABASE": "app\nother"},
		{"MARIADB_DATABASE": "app", "MARIADB_DATABASES_EXCLUDE": "other"},
		{"MARIADB_DATABASE": "app", "BACKUP_FILENAME_MODE": "wrong"},
		{"MARIADB_DATABASE": "app", "BACKUP_ALLOW_UNVERSIONED_OVERWRITE": "TRUE"},
		{"MARIADB_DATABASE": "app", "BACKUP_ALLOW_UNVERSIONED_OVERWRITE": ""},
		{"MARIADB_DATABASE": "app", "BACKUP_MODE": "wrong"},
	}
	for _, value := range []string{"0", "-1", "abc", "08", "1.5", "999999999999"} {
		for _, mode := range []string{"timestamp", "fixed"} {
			cases = append(cases, map[string]string{"MARIADB_DATABASE": "app", "BACKUP_FILENAME_MODE": mode, "BACKUP_KEEP_DAYS": value})
		}
	}
	for _, option := range []string{"--help", "--result-file=/tmp/dump.sql", "--all-databases", "--force", "--defaults-file=/tmp/other.cnf"} {
		cases = append(cases, map[string]string{"MARIADB_DATABASE": "app", "MARIADB_DUMP_EXTRA_OPTS": option})
	}
	for i, settings := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f := newFixture(t)
			for key, value := range settings {
				f.env[key] = value
			}
			f.run(false, "backup")
			assertEqual(t, len(f.processes("")), 0)
			assertEqual(t, len(f.s3.calls("")), 0)
		})
	}
}

func TestInvalidOrEmptyDiscoveryNeverDumps(t *testing.T) {
	for i, value := range [][]string{{}, {""}, {"a\nb"}} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_BACKUP_ALL"] = "true"
			f.databases(value)
			f.run(false, "backup")
			assertEqual(t, len(f.processes("mariadb-dump")), 0)
		})
	}
	for _, malformed := range []string{"not hex\n", "123\n", "ff\n"} {
		t.Run(malformed, func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_BACKUP_ALL"] = "true"
			if err := os.WriteFile(f.env["FAKE_DATABASES_FILE"], []byte(malformed), 0600); err != nil {
				t.Fatal(err)
			}
			f.run(false, "backup")
			assertEqual(t, len(f.processes("mariadb-dump")), 0)
		})
	}
	for _, failure := range []string{"discovery", "excluded"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_BACKUP_ALL"] = "true"
			if failure == "discovery" {
				f.env["FAKE_DISCOVERY_FAILURE"] = "true"
			} else {
				f.env["MARIADB_DATABASES_EXCLUDE"] = "app,analytics"
			}
			f.run(false, "backup")
			assertEqual(t, len(f.processes("mariadb-dump")), 0)
		})
	}
}

func TestFixedKeysEncodingEncryptionAndVersionRestore(t *testing.T) {
	for _, tc := range []struct{ name, directory string }{
		{"app", "app"}, {"app_2000-01-01T00:00:00", "app_2000-01-01T00%3A00%3A00"},
		{"with space", "with%20space"}, {"a/b", "a%2Fb"}, {"a%2Fb", "a%252Fb"}, {".", "."}, {"..", ".."},
	} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/encrypted=%t", tc.name, encrypted), func(t *testing.T) {
				f := newFixture(t)
				f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = tc.name, "fixed"
				suffix := ".sql"
				if encrypted {
					f.env["PASSPHRASE"], suffix = "test-passphrase", ".sql.gpg"
				}
				key := "backup/" + tc.directory + "_latest" + suffix
				f.run(true, "backup")
				assertEqual(t, keys(f.s3.calls("PUT")), []string{key})
				assertEqual(t, len(f.s3.calls("versioning")), 1)
				f.run(true, "restore", "--version-id", "opaque+/version==")
				gets := f.s3.calls("GET")
				assertEqual(t, keys(gets), []string{key})
				assertEqual(t, gets[0].Query.Get("versionId"), "opaque+/version==")
				assertEqual(t, f.databaseCalls("mariadb"), []string{tc.name})
				assertEqual(t, len(f.s3.calls("versioning")), 1)
				wantGPG := 0
				if encrypted {
					wantGPG = 2
				}
				assertEqual(t, len(f.processes("gpg")), wantGPG)
			})
		}
	}
}

func TestGPGPassphraseUsesPrivateDescriptor(t *testing.T) {
	for _, mode := range []string{"per-database", "combined"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			secret := " secret \"quoted\" \\ tab\treturn\r trailing "
			snapshot := filepath.Join(f.dir, "gpg-secret")
			f.env["PASSPHRASE"] = secret
			f.env["BACKUP_MODE"], f.env["BACKUP_FILENAME_MODE"] = mode, "fixed"
			f.env["MARIADB_DATABASES"] = "app,analytics"
			f.env["FAKE_GPG_PASSPHRASE_SNAPSHOT"] = snapshot
			output := f.run(true, "backup")
			data, err := os.ReadFile(snapshot)
			if err != nil || string(data) != secret {
				t.Fatalf("GPG did not read the complete passphrase: %v", err)
			}
			f.env["MARIADB_DATABASES"] = ""
			if mode == "per-database" {
				f.env["MARIADB_DATABASE"] = "app"
			}
			output += f.run(true, "restore")
			data, err = os.ReadFile(snapshot)
			if err != nil || string(data) != secret {
				t.Fatalf("GPG decrypt did not read the complete passphrase: %v", err)
			}
			calls := f.processes("gpg")
			wantCalls := 2
			if mode == "per-database" {
				wantCalls = 3
			}
			assertEqual(t, len(calls), wantCalls)
			for i, call := range calls {
				wantFD := "4"
				if i == len(calls)-1 {
					wantFD = "3"
				}
				assertEqual(t, call.Args[:2], []string{"--passphrase-fd", wantFD})
			}
			log, err := os.ReadFile(f.log)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(log)+output, secret) {
				t.Fatal("passphrase exposed in process arguments or logs")
			}
		})
	}
}

func TestGPGRejectsMultilinePassphrase(t *testing.T) {
	for _, command := range []string{"backup", "restore"} {
		f := newFixture(t)
		f.env["MARIADB_DATABASE"] = "app"
		f.env["PASSPHRASE"] = "first line\nsecret suffix"
		output := f.run(false, command)
		if !strings.Contains(output, "PASSPHRASE must not contain a newline") {
			t.Fatal("missing multiline passphrase validation")
		}
		assertEqual(t, len(f.processes("")), 0)
		assertEqual(t, len(f.s3.calls("")), 0)
	}
}

func TestFixedFilenamePolicy(t *testing.T) {
	for _, status := range []string{"Enabled", "Suspended", "", "AccessDenied"} {
		for _, override := range []string{"false", "true"} {
			t.Run(status+"/override="+override, func(t *testing.T) {
				f := newFixture(t)
				f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = "app", "fixed"
				f.env["BACKUP_ALLOW_UNVERSIONED_OVERWRITE"] = override
				f.s3.versioning = status
				if status == "AccessDenied" {
					f.s3.fail = "versioning"
				}
				output := f.run(true, "backup")
				assertEqual(t, len(f.s3.calls("versioning")), 1)
				puts := f.s3.calls("PUT")
				assertEqual(t, len(puts), 1)
				fallback := status != "Enabled" && override == "false"
				assertEqual(t, puts[0].Key == "backup/app_latest.sql", !fallback)
				assertEqual(t, strings.Contains(output, "Restore command:"), fallback)
				assertEqual(t, strings.Contains(output, "Warning:"), status != "Enabled")
				if status != "Enabled" && override == "true" && !strings.Contains(output, "overwrite the previous backup") {
					t.Fatal(output)
				}
				if status == "AccessDenied" && !strings.Contains(output, "AccessDenied") {
					t.Fatal(output)
				}
			})
		}
	}
}

func TestFallbackRestoreCommand(t *testing.T) {
	for _, mode := range []string{"per-database", "combined"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.s3.fail = "versioning"
			f.env["BACKUP_MODE"], f.env["BACKUP_FILENAME_MODE"] = mode, "fixed"
			f.env["MARIADB_DATABASES"] = "app,team's db"
			f.env["PASSPHRASE"] = "test-passphrase"
			f.env["PATH"] = filepath.Dir(cliBinary) + string(os.PathListSeparator) + f.env["PATH"]
			output := f.run(true, "backup")
			assertEqual(t, len(f.s3.calls("versioning")), 1)
			puts := f.s3.calls("PUT")
			wantCount := 2
			if mode == "combined" {
				wantCount = 1
			}
			assertEqual(t, len(puts), wantCount)
			var commands []string
			for _, line := range strings.Split(output, "\n") {
				if command, ok := strings.CutPrefix(line, "Restore command: "); ok {
					commands = append(commands, command)
				}
			}
			assertEqual(t, len(commands), wantCount)
			for i, command := range commands {
				if !strings.Contains(output, fmt.Sprintf("Saved fallback S3 key: %q", puts[i].Key)) || !strings.HasSuffix(puts[i].Key, ".sql.gpg") {
					t.Fatal(output)
				}
				cmd := f.command()
				cmd.Path, cmd.Args = "/bin/sh", []string{"sh", "-c", command}
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("restore command failed: %v\n%s", err, out)
				}
			}
			assertEqual(t, keys(f.s3.calls("GET")), keys(puts))
			if mode == "per-database" {
				assertEqual(t, f.databaseCalls("mariadb"), []string{"app", "team's db"})
			}
		})
	}
}

func TestFallbackUploadFailure(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = "app", "fixed"
	f.s3.fail, f.s3.uploadFailure = "versioning", ".sql"
	output := f.run(false, "backup")
	assertEqual(t, len(f.s3.calls("PUT")), 1)
	if strings.Contains(output, "Restore command:") || !strings.Contains(output, "1 failed") {
		t.Fatal(output)
	}
}

func TestTimestampIgnoresOverwriteOverride(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["BACKUP_ALLOW_UNVERSIONED_OVERWRITE"] = "app", "true"
	f.run(true, "backup")
	assertEqual(t, len(f.s3.calls("versioning")), 0)
	puts := f.s3.calls("PUT")
	assertEqual(t, len(puts), 1)
	assertEqual(t, strings.HasPrefix(puts[0].Key, "backup/app/"), true)
}

func TestFailuresContinueOtherDatabasesAndSkipUnsafeRetention(t *testing.T) {
	for _, mode := range []string{"timestamp", "fixed"} {
		for _, stage := range []string{"dump", "empty", "empty-encrypted", "encrypt", "upload"} {
			t.Run(mode+"/"+stage, func(t *testing.T) {
				f := newFixture(t)
				f.env["MARIADB_DATABASES"], f.env["BACKUP_FILENAME_MODE"] = "app,billing", mode
				f.env["PASSPHRASE"], f.env["BACKUP_KEEP_DAYS"] = "test", "7"
				switch stage {
				case "dump":
					f.env["FAKE_DUMP_FAILURE"] = "app"
				case "empty":
					f.env["PASSPHRASE"] = ""
					fallthrough
				case "empty-encrypted":
					f.env["FAKE_EMPTY_DUMP"] = "app"
				case "encrypt":
					f.env["FAKE_GPG_FAILURE"] = "app"
				case "upload":
					f.s3.uploadFailure = "app"
				}
				output := f.run(false, "backup")
				if strings.HasPrefix(stage, "empty") && !strings.Contains(output, "no backup data to upload") {
					t.Fatalf("missing empty backup error: %s", output)
				}
				if !strings.Contains(output, "1 succeeded, 1 failed") {
					t.Fatalf("incorrect failure summary: %s", output)
				}
				assertEqual(t, f.databaseCalls("mariadb-dump"), []string{"app", "billing"})
				assertEqual(t, len(f.s3.calls("list")), 0)
				uploads := f.s3.calls("PUT")
				want := 1
				if stage == "upload" {
					want = 2
				}
				assertEqual(t, len(uploads), want)
				if !strings.Contains(uploads[len(uploads)-1].Key, "billing") {
					t.Fatalf("later database was not uploaded: %v", keys(uploads))
				}
			})
		}
	}
}

func TestCustomEndpointUsesBucketLifecycle(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["BACKUP_KEEP_DAYS"] = "7"
	f.s3.objects = []object{{Key: "backup/app/2000-01-01T00:00:00.sql", LastModified: "2000-01-01T00:00:00Z"}}
	out := f.run(true, "backup")
	if !strings.Contains(out, "7-day bucket lifecycle rule") {
		t.Fatal(out)
	}
	assertEqual(t, len(f.s3.calls("PUT")), 1)
	assertEqual(t, len(f.s3.calls("list")), 0)
	assertEqual(t, len(f.s3.calls("DELETE")), 0)
}

func TestPrefixLayoutsNormalizeTrailingSlashes(t *testing.T) {
	for _, tc := range []struct{ prefix, directory string }{
		{"backup/", "backup/app/"}, {"backup//", "backup/app/"},
		{"", "app/"}, {"/", "app/"}, {"/backup/", "/backup/app/"},
	} {
		for _, mode := range []string{"timestamp", "fixed"} {
			t.Run(fmt.Sprintf("%q/%s", tc.prefix, mode), func(t *testing.T) {
				f := newFixture(t)
				f.env["MARIADB_DATABASE"], f.env["S3_PREFIX"], f.env["BACKUP_FILENAME_MODE"] = "app", tc.prefix, mode
				f.env["BACKUP_KEEP_DAYS"] = "7"
				oldKey := tc.directory + "2000-01-01T00:00:00.sql"
				f.s3.objects = []object{{Key: oldKey, LastModified: "2000-01-01T00:00:00Z"}}
				f.run(true, "backup")
				assertEqual(t, len(f.s3.calls("DELETE")), 0)
				if mode == "fixed" {
					key := strings.TrimSuffix(tc.directory, "app/") + "app_latest.sql"
					assertEqual(t, keys(f.s3.calls("PUT")), []string{key})
					f.run(true, "restore")
					assertEqual(t, keys(f.s3.calls("GET")), []string{key})
				} else {
					if !strings.HasPrefix(f.s3.calls("PUT")[0].Key, tc.directory) {
						t.Fatalf("timestamp prefix changed: %v", keys(f.s3.calls("PUT")))
					}
					f.run(true, "restore", "2000-01-01T00:00:00")
					assertEqual(t, keys(f.s3.calls("GET")), []string{oldKey})
				}
			})
		}
	}
}

func TestLatestRestoreExactMatchingAndPagination(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.s3.pageSize = 1000
	for i := 0; i < 1001; i++ {
		key := "backup/app/" + time.Date(2020, 1, 1, 0, 0, i, 0, time.UTC).Format("2006-01-02T15:04:05") + ".sql"
		f.s3.objects = append(f.s3.objects, object{Key: key, LastModified: "2020-01-01T00:00:00Z"})
	}
	want := strings.TrimSuffix(f.s3.objects[1000].Key, ".sql") + ".123456789.sql"
	f.s3.objects = append(f.s3.objects, object{Key: "backup/app/2099-01-01T00:00:00.sql", LastModified: "2010-01-01T00:00:00Z"})
	f.s3.objects[1000].LastModified = "2025-01-01T00:00:00Z"
	for _, key := range []string{want, "backup/app/extra_2030-01-01T00:00:00.sql", "backup/app/2025-01-01T00:00:00.sql.gpg", "backup/app/2040-01-01T00%3A00%3A00/latest.sql", "backup/app/9999-99-99T99:99:99.sql"} {
		f.s3.objects = append(f.s3.objects, object{Key: key, LastModified: "2025-01-01T00:00:00Z"})
	}
	f.run(true, "restore")
	assertEqual(t, len(f.s3.calls("list")), 2)
	assertEqual(t, keys(f.s3.calls("GET")), []string{want})
	assertEqual(t, f.databaseCalls("mariadb"), []string{"app"})
	for _, arg := range f.processes("mariadb")[0].Args {
		if arg == "--force" || arg == "-f" {
			t.Fatal("mariadb must stop on SQL errors")
		}
	}
}

func TestExplicitRestoreAcceptsNanosecondTimestamp(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	timestamp := "2020-01-01T00:00:00.123456789"
	f.run(true, "restore", timestamp)
	assertEqual(t, keys(f.s3.calls("GET")), []string{"backup/app/" + timestamp + ".sql"})
}

func TestBackupModesGroupSelectedDatabases(t *testing.T) {
	for _, mode := range []string{"", "per-database", "combined"} {
		for _, all := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/all=%t", mode, all), func(t *testing.T) {
				f := newFixture(t)
				f.env["BACKUP_MODE"], f.env["BACKUP_FILENAME_MODE"] = mode, "fixed"
				if all {
					f.env["MARIADB_BACKUP_ALL"], f.env["MARIADB_DATABASES_EXCLUDE"] = "true", "scratch"
					f.databases([]string{"app", "billing", "scratch"})
				} else {
					f.env["MARIADB_DATABASES"] = "app,billing,app"
				}
				f.run(true, "backup")
				assertEqual(t, f.databaseCalls("mariadb-dump"), []string{"app", "billing"})
				wantCalls := 2
				wantKeys := []string{"backup/app_latest.sql", "backup/billing_latest.sql"}
				if mode == "combined" {
					wantCalls = 1
					wantKeys = []string{"backup/latest.sql"}
					assertEqual(t, f.s3.calls("PUT")[0].Payload, "-- SQL [app] [billing]\n")
				}
				calls := f.processes("mariadb-dump")
				assertEqual(t, len(calls), wantCalls)
				assertEqual(t, keys(f.s3.calls("PUT")), wantKeys)
				for _, call := range calls {
					joined := " " + strings.Join(call.Args, " ") + " "
					assertEqual(t, strings.Contains(joined, " --databases "), mode == "combined")
					for _, required := range []string{"--single-transaction", "--quick", "--routines", "--events", "--triggers", "--hex-blob"} {
						if !strings.Contains(joined, " "+required+" ") {
							t.Errorf("missing dump option %s: %v", required, call.Args)
						}
					}
				}
			})
		}
	}
}

func TestCombinedRestoreReadsSQLWithoutDatabaseSelection(t *testing.T) {
	for _, mode := range []string{"timestamp", "fixed"} {
		for _, encrypted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/encrypted=%t", mode, encrypted), func(t *testing.T) {
				f := newFixture(t)
				f.env["BACKUP_MODE"], f.env["BACKUP_FILENAME_MODE"] = "combined", mode
				f.env["FAKE_RESTORE_INPUT"] = filepath.Join(f.dir, "restored.sql")
				suffix := ".sql"
				if encrypted {
					f.env["PASSPHRASE"], suffix = "test", ".sql.gpg"
				}
				key := "backup/2020-01-01T00:00:00" + suffix
				args := []string{"restore", "2020-01-01T00:00:00"}
				if mode == "fixed" {
					key = "backup/latest" + suffix
					args = []string{"restore", "--version-id", "opaque+/version=="}
				}
				f.run(true, args...)
				assertEqual(t, keys(f.s3.calls("GET")), []string{key})
				if mode == "fixed" {
					assertEqual(t, f.s3.calls("GET")[0].Query.Get("versionId"), "opaque+/version==")
				}
				calls := f.processes("mariadb")
				assertEqual(t, len(calls), 1)
				for _, arg := range calls[0].Args {
					if strings.HasPrefix(arg, "--database") {
						t.Fatalf("combined restore must use the database selections in the SQL: %v", calls[0].Args)
					}
				}
				payload, err := os.ReadFile(f.env["FAKE_RESTORE_INPUT"])
				if err != nil {
					t.Fatal(err)
				}
				assertEqual(t, string(payload), "-- SQL restored\n")
			})
		}
	}
}

func TestCombinedRestoreRejectsMisleadingDatabaseFilters(t *testing.T) {
	for _, setting := range []string{"MARIADB_DATABASE", "MARIADB_DATABASES", "MARIADB_DATABASES_EXCLUDE", "MARIADB_BACKUP_ALL"} {
		t.Run(setting, func(t *testing.T) {
			f := newFixture(t)
			f.env["BACKUP_MODE"], f.env["BACKUP_FILENAME_MODE"], f.env[setting] = "combined", "fixed", "app"
			if setting == "MARIADB_BACKUP_ALL" {
				f.env[setting] = "true"
			}
			f.run(false, "restore")
			assertEqual(t, len(f.processes("")), 0)
			assertEqual(t, len(f.s3.calls("")), 0)
		})
	}
}

func TestCombinedRetentionAndLatestRestoreStayWithinMode(t *testing.T) {
	for _, mode := range []string{"timestamp", "fixed"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_DATABASES"], f.env["BACKUP_MODE"], f.env["BACKUP_FILENAME_MODE"], f.env["BACKUP_KEEP_DAYS"] = "app,billing", "combined", mode, "7"
			expired := []string{"backup/2000-01-01T00:00:00.sql", "backup/2000-01-01T00:00:01.sql.gpg"}
			for _, key := range append(append([]string{}, expired...),
				"backup/app/2000-01-01T00:00:00.sql", "backup/latest.sql",
				"backup/2000-01-01T00:00:00.sql.extra", "backup/notes.sql") {
				f.s3.objects = append(f.s3.objects, object{Key: key, LastModified: "2025-01-01T00:00:00Z"})
			}
			latest := "backup/2020-01-01T00:00:00.sql"
			for _, key := range []string{latest, "backup/2030-01-01T00:00:00.sql.gpg", "backup/app/2040-01-01T00:00:00.sql"} {
				f.s3.objects = append(f.s3.objects, object{Key: key, LastModified: time.Now().UTC().Format(time.RFC3339)})
			}
			f.run(true, "backup")
			assertEqual(t, len(f.s3.calls("DELETE")), 0)
			delete(f.env, "MARIADB_DATABASES")
			f.run(true, "restore")
			if mode == "fixed" {
				latest = "backup/latest.sql"
			}
			assertEqual(t, keys(f.s3.calls("GET")), []string{latest})
		})
	}
}

func TestCombinedFailureSkipsRetentionAndDoesNotSplitDump(t *testing.T) {
	for _, stage := range []string{"dump", "encrypt", "upload"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_DATABASES"], f.env["BACKUP_MODE"], f.env["BACKUP_KEEP_DAYS"], f.env["PASSPHRASE"] = "app,billing", "combined", "7", "test"
			switch stage {
			case "dump":
				f.env["FAKE_DUMP_FAILURE"] = "app"
			case "encrypt":
				f.env["FAKE_GPG_FAILURE"] = "billing"
			case "upload":
				f.s3.fail = "PUT"
			}
			f.run(false, "backup")
			assertEqual(t, len(f.processes("mariadb-dump")), 1)
			assertEqual(t, f.databaseCalls("mariadb-dump"), []string{"app", "billing"})
			assertEqual(t, len(f.s3.calls("list")), 0)
			assertEqual(t, len(f.s3.calls("DELETE")), 0)
		})
	}
}

func TestCredentialsArePrivateAndRemovedAfterSuccessOrFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(strconv.FormatBool(failure), func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_DATABASE"], f.env["MARIADB_PASSWORD"] = "app", "pass\\word\"\n[client]\nuser=other"
			f.env["FAKE_DEFAULTS_SNAPSHOT"] = filepath.Join(f.dir, "defaults.cnf")
			if failure {
				f.env["FAKE_DUMP_FAILURE"] = "app"
			}
			output := f.run(!failure, "backup")
			if strings.Contains(output, f.env["MARIADB_PASSWORD"]) {
				t.Fatal("password exposed in command output")
			}
			calls := f.processes("mariadb-dump")
			assertEqual(t, len(calls), 1)
			for _, arg := range calls[0].Args {
				if strings.Contains(arg, f.env["MARIADB_PASSWORD"]) || strings.HasPrefix(arg, "--password") {
					t.Fatal("password exposed in command arguments")
				}
			}
			credentials := strings.TrimPrefix(calls[0].Args[0], "--defaults-file=")
			if !strings.HasPrefix(credentials, "/proc/self/fd/") {
				t.Fatal("credential has a persistent pathname")
			}
			data, err := os.ReadFile(f.env["FAKE_DEFAULTS_SNAPSHOT"])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), "\n[client]") != 0 || strings.Contains(string(data), "\nuser=other") {
				t.Fatal("password escaped out of its option-file value")
			}
		})
	}
}

func TestRestoreFailuresNeverReportSuccess(t *testing.T) {
	for _, stage := range []string{"download", "decrypt", "restore", "list", "empty"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_DATABASE"] = "app"
			if stage == "list" || stage == "empty" {
				if stage == "list" {
					f.s3.fail = "list"
				}
			} else {
				f.env["BACKUP_FILENAME_MODE"], f.env["PASSPHRASE"] = "fixed", "test"
				switch stage {
				case "download":
					f.s3.fail = "GET"
				case "decrypt":
					f.env["FAKE_GPG_FAILURE"] = "restored"
				case "restore":
					f.env["FAKE_RESTORE_FAILURE"] = "true"
				}
			}
			output := f.run(false, "restore")
			if strings.Contains(output, "Restore complete") {
				t.Fatalf("restore incorrectly reported success: %s", output)
			}
			want := 0
			if stage == "restore" {
				want = 1
			}
			assertEqual(t, len(f.processes("mariadb")), want)
		})
	}
}

func TestRestoreInvalidArgumentsNeverAccessServices(t *testing.T) {
	for i, tc := range []struct {
		database, mode string
		args           []string
	}{
		{"", "timestamp", nil}, {"app", "timestamp", []string{"--version-id", "id"}},
		{"app", "timestamp", []string{"bad-timestamp"}}, {"app", "timestamp", []string{"2020-01-01T00:00:00\n"}},
		{"app", "timestamp", []string{"2021-02-29T00:00:00"}}, {"app", "timestamp", []string{"2020-01-01T24:00:00.123456789"}},
		{"app", "fixed", []string{"2020-01-01T00:00:00"}}, {"app", "fixed", []string{"--version-id", ""}},
		{"app", "timestamp", []string{"a", "b", "c"}},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f := newFixture(t)
			f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"] = tc.database, tc.mode
			f.run(false, append([]string{"restore"}, tc.args...)...)
			assertEqual(t, len(f.processes("")), 0)
			assertEqual(t, len(f.s3.calls("")), 0)
		})
	}
}

func TestBackupLockCoversDumpAndUploadAcrossProcesses(t *testing.T) {
	for _, stage := range []string{"dump", "PUT"} {
		t.Run(stage, func(t *testing.T) {
			first := newFixture(t)
			first.env["MARIADB_DATABASE"], first.env["BACKUP_KEEP_DAYS"] = "app", "7"
			first.s3.objects = []object{{Key: "backup/app/2000-01-01T00:00:00.sql", LastModified: "2000-01-01T00:00:00Z"}}
			entered, release := filepath.Join(first.dir, "entered"), filepath.Join(first.dir, "release")
			if stage == "dump" {
				first.env["FAKE_PAUSE_COMMAND"], first.env["FAKE_PAUSE_ENTERED"], first.env["FAKE_PAUSE_RELEASE"] = "mariadb-dump", entered, release
			} else {
				first.s3.pauseMethod, first.s3.pauseEntered, first.s3.pauseRelease = stage, make(chan struct{}), make(chan struct{})
			}
			var output bytes.Buffer
			cmd := first.command("backup")
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			releaseBackup := func() {
				releaseOnce.Do(func() {
					if stage == "dump" {
						os.WriteFile(release, nil, 0600)
					} else {
						close(first.s3.pauseRelease)
					}
				})
			}
			done := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = cmd.Wait()
				close(done)
			}()
			t.Cleanup(func() {
				releaseBackup()
				select {
				case <-done:
				default:
					cmd.Process.Kill()
					<-done
				}
			})
			if stage == "dump" {
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(entered); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("first backup did not enter paused dump")
					}
					time.Sleep(10 * time.Millisecond)
				}
			} else {
				select {
				case <-first.s3.pauseEntered:
				case <-time.After(5 * time.Second):
					t.Fatal("first backup did not enter paused S3 request")
				}
			}
			second := newFixture(t)
			second.env["MARIADB_DATABASE"] = "app"
			second.env["S3_ENDPOINT"] = first.env["S3_ENDPOINT"]
			secondOutput := second.run(false, "backup")
			if !strings.Contains(secondOutput, "backup lock") {
				t.Fatalf("overlap failed for a reason other than the shared lock: %s", secondOutput)
			}
			assertEqual(t, len(second.processes("")), 0)
			assertEqual(t, len(second.s3.calls("")), 0)
			releaseBackup()
			select {
			case <-done:
				if waitErr != nil {
					t.Fatalf("first backup failed: %v\n%s", waitErr, output.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("first backup did not finish after release")
			}
			first.assertClean()
			second.run(true, "backup")
		})
	}
}

type backgroundCLI struct {
	cmd    *exec.Cmd
	done   chan struct{}
	err    error
	output string
}

func (f *fixture) background(args ...string) *backgroundCLI {
	f.t.Helper()
	log, err := os.CreateTemp(f.dir, "background-*.log")
	if err != nil {
		f.t.Fatal(err)
	}
	p := &backgroundCLI{cmd: f.command(args...), done: make(chan struct{}), output: log.Name()}
	// A file avoids tying Wait to stderr inherited by a deliberately orphaned
	// child in the SIGKILL test. Process completion is independently observable.
	p.cmd.Stdout, p.cmd.Stderr = log, log
	if err := p.cmd.Start(); err != nil {
		log.Close()
		f.t.Fatal(err)
	}
	log.Close()
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	f.t.Cleanup(func() {
		select {
		case <-p.done:
			return
		default:
		}
		p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

func (p *backgroundCLI) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-p.done:
		return p.err
	case <-time.After(5 * time.Second):
		output, _ := os.ReadFile(p.output)
		t.Fatalf("CLI did not terminate\n%s", output)
		return nil
	}
}

func waitForFile(t *testing.T, path string, p *backgroundCLI) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-p.done:
			output, _ := os.ReadFile(p.output)
			t.Fatalf("CLI exited before creating %s: %v\n%s", path, p.err, output)
		default:
		}
		if time.Now().After(deadline) {
			output, _ := os.ReadFile(p.output)
			t.Fatalf("CLI did not reach paused command\n%s", output)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestScheduledBackupSIGTERMKillsChildAndCleansWorkspace(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["SCHEDULE"] = "app", "@every 1s"
	entered, release := filepath.Join(f.dir, "entered"), filepath.Join(f.dir, "release")
	pidPath := filepath.Join(f.dir, "child.pid")
	f.env["FAKE_PAUSE_COMMAND"], f.env["FAKE_PAUSE_ENTERED"], f.env["FAKE_PAUSE_RELEASE"], f.env["FAKE_CHILD_PID"] = "mariadb-dump", entered, release, pidPath
	p := f.background("run")
	t.Cleanup(func() { os.WriteFile(release, nil, 0600) })
	waitForFile(t, entered, p)
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(t); err == nil {
		t.Fatal("interrupted scheduled backup returned success")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 143 {
		t.Fatalf("SIGTERM exit = %v, want 143", err)
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("dump process %d survived scheduler shutdown: %v", pid, err)
	}
	f.assertClean()
	assertEqual(t, len(f.processes("mariadb-dump")), 1)
	assertEqual(t, len(f.s3.calls("PUT")), 0)
	next := newFixture(t)
	next.env["MARIADB_DATABASE"] = "app"
	next.env["S3_ENDPOINT"] = f.env["S3_ENDPOINT"]
	next.run(true, "backup")
}

func TestAbruptParentExitKeepsBackupLockUntilDumpChildExits(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	entered, release := filepath.Join(f.dir, "entered"), filepath.Join(f.dir, "release")
	pidPath := filepath.Join(f.dir, "child.pid")
	f.env["FAKE_PAUSE_COMMAND"], f.env["FAKE_PAUSE_ENTERED"], f.env["FAKE_PAUSE_RELEASE"], f.env["FAKE_CHILD_PID"] = "mariadb-dump", entered, release, pidPath
	p := f.background("backup")
	t.Cleanup(func() { os.WriteFile(release, nil, 0600) })
	waitForFile(t, entered, p)
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-pid, syscall.SIGKILL) })
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(t); err == nil {
		t.Fatal("SIGKILLed backup returned success")
	}
	next := newFixture(t)
	next.env["MARIADB_DATABASE"] = "app"
	next.env["S3_ENDPOINT"] = f.env["S3_ENDPOINT"]
	output := next.run(false, "backup")
	if !strings.Contains(output, "backup lock") {
		t.Fatalf("orphaned dump did not retain the lock: %s", output)
	}
	assertEqual(t, len(next.processes("")), 0)
	assertEqual(t, len(next.s3.calls("")), 0)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		output, err := next.command("backup").CombinedOutput()
		if err == nil {
			break
		}
		if time.Now().After(deadline) || !strings.Contains(string(output), "backup lock") {
			t.Fatalf("backup lock was not released after the dump child exited: %v\n%s", err, output)
		}
		time.Sleep(10 * time.Millisecond)
	}
	next.assertClean()
	assertEqual(t, len(next.processes("mariadb-dump")), 1)
	assertEqual(t, len(f.s3.calls("PUT")), 1)
	f.assertClean()
}

func TestTimeoutReleasesBackupLock(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["BACKUP_TIMEOUT"] = "500ms"
	f.env["FAKE_PAUSE_COMMAND"] = "mariadb-dump"
	f.env["FAKE_PAUSE_ENTERED"] = filepath.Join(f.dir, "entered")
	f.env["FAKE_PAUSE_RELEASE"] = filepath.Join(f.dir, "release")
	if out := f.run(false, "backup"); !strings.Contains(out, "deadline exceeded") {
		t.Fatal(out)
	}
	assertEqual(t, len(f.s3.calls("PUT")), 0)
	delete(f.env, "FAKE_PAUSE_COMMAND")
	delete(f.env, "BACKUP_TIMEOUT")
	f.run(true, "backup")
}

func TestRestoreRejectsEmptyPlaintext(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"], f.env["BACKUP_FILENAME_MODE"], f.env["PASSPHRASE"] = "app", "fixed", "pass"
	if err := os.WriteFile(filepath.Join(f.dir, "bin", "gpg"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if out := f.run(false, "restore"); !strings.Contains(out, "empty SQL backup") {
		t.Fatal(out)
	}
	assertEqual(t, len(f.processes("mariadb")), 0)
}

func TestExternalDataRequiresExplicitPolicy(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["FAKE_TABLE_METADATA"] = "617070\t72656D6F7465\tFEDERATED\n"
	if out := f.run(false, "backup"); !strings.Contains(out, "app.remote") {
		t.Fatal(out)
	}
	assertEqual(t, len(f.processes("mariadb-dump")), 0)
	for _, option := range []string{"--ignore-table-data=app.remote", "--no-data-med=0"} {
		f.env["MARIADB_DUMP_EXTRA_OPTS"] = option
		f.run(true, "backup")
	}
}

func TestRestoreIgnoresBackupOnlyConfiguration(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["S3_UPLOAD_BUFFER"] = "invalid"
	f.env["S3_UPLOAD_PART_SIZE_MB"] = "invalid"
	f.env["BACKUP_TIMEOUT"] = "invalid"
	f.env["BACKUP_ALLOW_UNVERSIONED_OVERWRITE"] = "invalid"
	f.run(true, "restore", "2026-01-01T00:00:00")
	assertEqual(t, f.databaseCalls("mariadb"), []string{"app"})
}

func TestServerLockContentionStopsBackupAndRestoreBeforeS3(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["FAKE_LOCK_RESULT"] = "0"
	for _, command := range []string{"backup", "restore"} {
		f.env["S3_PREFIX"] = command
		if out := f.run(false, command); !strings.Contains(out, "server is busy") {
			t.Fatal(out)
		}
	}
	assertEqual(t, len(f.s3.requests), 0)
	assertEqual(t, len(f.processes("mariadb-dump")), 0)
	assertEqual(t, len(f.databaseCalls("mariadb")), 0)
}

func TestPreflightChecksManyDiscoveredDatabases(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = ""
	f.env["MARIADB_BACKUP_ALL"] = "true"
	f.env["BACKUP_MODE"] = "combined"
	var databases []string
	for i := range 450 {
		databases = append(databases, fmt.Sprintf("%064d", i))
	}
	f.databases(databases)
	f.run(true, "backup")
	assertEqual(t, len(f.processes("metadata")), 15)
	assertEqual(t, len(f.processes("mariadb-dump")), 1)
}

func TestDataExclusionStillRequiresTriggerPrivilege(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["FAKE_TABLE_METADATA"] = "617070\t6c6f67\tmissing TRIGGER\n"
	f.env["MARIADB_DUMP_EXTRA_OPTS"] = "--ignore-table-data=app.log"
	if out := f.run(false, "backup"); !strings.Contains(out, "TRIGGER") {
		t.Fatal(out)
	}
	f.env["MARIADB_DUMP_EXTRA_OPTS"] = "--ignore-table=app.log"
	f.run(true, "backup")
}

func TestLockConnectionLossStopsRunningDump(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["FAKE_LOCK_PID"] = filepath.Join(f.dir, "lock.pid")
	f.env["FAKE_PAUSE_COMMAND"] = "mariadb-dump"
	f.env["FAKE_PAUSE_ENTERED"] = filepath.Join(f.dir, "entered")
	f.env["FAKE_PAUSE_RELEASE"] = filepath.Join(f.dir, "release")
	p := f.background("backup")
	waitForFile(t, f.env["FAKE_PAUSE_ENTERED"], p)
	data, err := os.ReadFile(f.env["FAKE_LOCK_PID"])
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := p.wait(t); err == nil {
		t.Fatal("backup continued after lock connection loss")
	}
	assertEqual(t, completedUploads(f), 0)
}

func TestEncryptedRestoreLeavesNoNamedSQLAfterAbruptExit(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["BACKUP_FILENAME_MODE"] = "fixed"
	f.env["PASSPHRASE"] = "test passphrase"
	f.env["FAKE_PAUSE_COMMAND"] = "mariadb"
	f.env["FAKE_PAUSE_ENTERED"] = filepath.Join(f.dir, "entered")
	release := filepath.Join(f.dir, "release")
	f.env["FAKE_PAUSE_RELEASE"] = release
	p := f.background("restore")
	t.Cleanup(func() { os.WriteFile(release, nil, 0600) })
	waitForFile(t, f.env["FAKE_PAUSE_ENTERED"], p)
	f.assertClean()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if p.wait(t) == nil {
		t.Fatal("killed restore returned success")
	}
	f.assertClean()
}

func TestVisibilityWarningsRemainAdvisory(t *testing.T) {
	f := newFixture(t)
	f.env["MARIADB_DATABASE"] = "app"
	f.env["FAKE_ROLE_LOOKUP_FAILURE"] = "true"
	f.env["FAKE_TABLE_METADATA"] = "617070\t\tmissing SELECT\n617070\t\tmissing routine access\n"
	out := f.run(true, "backup")
	if !strings.Contains(out, "schema-wide SELECT") || !strings.Contains(out, "routine definitions") || len(f.s3.calls("PUT")) != 1 {
		t.Fatalf("expected both warnings and one published backup: %s", out)
	}
}
