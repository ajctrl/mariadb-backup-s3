package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/ajctrl/mariadb-backup-s3/internal/storage"
	"github.com/ajctrl/mariadb-backup-s3/internal/tempfile"
	"github.com/robfig/cron/v3"
)

type App struct {
	Config   Config
	Store    storage.Store
	Out, Err io.Writer
	now      func() time.Time
	lockPath string
}

func New(c Config, store storage.Store, out, stderr io.Writer) *App {
	scope := sha256.Sum256([]byte(c.Storage.Bucket + "\x00" + c.keyPrefix() + "\x00" + c.Storage.Endpoint))
	dir := filepath.Join("/tmp", fmt.Sprintf("mariadb-backup-s3-locks-%d", os.Geteuid()))
	return &App{Config: c, Store: store, Out: out, Err: stderr, now: time.Now, lockPath: filepath.Join(dir, fmt.Sprintf("%x.lock", scope))}
}

func parseSchedule(spec string) (cron.Schedule, error) {
	spec = strings.Join(strings.Fields(spec), " ")
	if (strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=")) && !strings.ContainsRune(spec, ' ') {
		return nil, fmt.Errorf("missing cron expression after timezone")
	}
	fields := strings.Fields(spec)
	if len(fields) >= 2 && fields[len(fields)-2] == "@every" {
		delay, err := time.ParseDuration(fields[len(fields)-1])
		if err != nil || delay < time.Second {
			return nil, errors.New("@every requires a duration of at least one second")
		}
	}
	parser := cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	return parser.Parse(spec)
}

func (a *App) Run(ctx context.Context) error {
	if a.Config.Schedule == "" {
		return a.Backup(ctx)
	}
	if _, err := a.Config.validateBackup(); err != nil {
		return err
	}
	schedule, err := parseSchedule(a.Config.Schedule)
	if err != nil {
		return fmt.Errorf("invalid SCHEDULE: %w", err)
	}
	if schedule.Next(a.now()).IsZero() {
		return fmt.Errorf("SCHEDULE has no future execution time")
	}
	scheduler := cron.New()
	scheduler.Schedule(schedule, cron.FuncJob(func() {
		if err := a.Backup(ctx); err != nil {
			fmt.Fprintf(a.Err, "Scheduled backup failed: %v\n", err)
		}
	}))
	fmt.Fprintf(a.Out, "Scheduling backups: %s\n", a.Config.Schedule)
	scheduler.Start()
	<-ctx.Done()
	// Cancellation reaches SDK requests and subprocess groups. Wait for cleanup
	// and lock release before PID 1 exits.
	<-scheduler.Stop().Done()
	return ctx.Err()
}

func (a *App) withRunner(lock *os.File, action func(commandRunner) error) (err error) {
	if strings.ContainsRune(a.Config.Passphrase, '\n') {
		return fmt.Errorf("PASSPHRASE must not contain a newline.")
	}
	return action(commandRunner{
		defaults:   "[client]\npassword=" + quoteOption(a.Config.Password) + "\n",
		passphrase: a.Config.Passphrase, stderr: a.Err, lock: lock,
	})
}

func quoteOption(value string) string {
	escape := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t", "\b", "\\b")
	return "\"" + escape.Replace(value) + "\""
}

func (a *App) Backup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if a.Config.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.Config.Timeout)
		defer cancel()
	}
	lock, err := acquireLock(a.lockPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	return a.withRunner(lock, func(runner commandRunner) error { return a.backup(ctx, runner) })
}

func (a *App) backup(ctx context.Context, runner commandRunner) error {
	days, err := a.Config.validateBackup()
	if err != nil {
		return err
	}
	return a.withServerLock(ctx, runner, false, func(ctx context.Context, runner commandRunner) error {
		return a.backupLocked(ctx, runner, days)
	})
}

func (a *App) backupLocked(ctx context.Context, runner commandRunner, days int) error {
	if days > 0 && !a.Store.ManagesRetention() {
		fmt.Fprintf(a.Err, "Custom S3 endpoint: configure a %d-day bucket lifecycle rule; application retention is disabled.\n", days)
		days = 0
	}
	// A scheduled run must check again even if the previous run fell back.
	naming := a.Config
	if naming.FilenameMode == "fixed" {
		enabled, err := a.Store.VersioningEnabled(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || !enabled {
			reason := "S3 bucket versioning is not enabled"
			if err != nil {
				reason = fmt.Sprintf("Could not verify S3 bucket versioning: %v", err)
			}
			if naming.AllowUnversionedOverwrite {
				fmt.Fprintf(a.Err, "Warning: %s. Using fixed filenames because BACKUP_ALLOW_UNVERSIONED_OVERWRITE=true. This may overwrite the previous backup.\n", reason)
			} else {
				naming.FilenameMode = "timestamp"
				fmt.Fprintf(a.Err, "Warning: %s. Using timestamped filenames for this run.\n", reason)
			}
		}
	}
	databases, err := a.databases(ctx, runner)
	if err != nil {
		return err
	}
	groups := make([][]string, len(databases))
	for i, database := range databases {
		groups[i] = []string{database}
	}
	if a.Config.BackupMode == "combined" {
		groups = [][]string{databases}
	}
	succeeded, failed, cleanupFailed := 0, 0, 0
	for _, group := range groups {
		database := group[0]
		label := strings.Join(group, ", ")
		if ctx.Err() != nil {
			break
		}
		backupTime := a.now()
		key := naming.backupKey(database, backupTime)
		if err := a.backupDatabases(ctx, runner, group, key, naming.FilenameMode == "timestamp"); err != nil {
			fmt.Fprintf(a.Err, "Backup failed: %s: %v\n", label, err)
			failed++
		} else {
			succeeded++
			if naming.FilenameMode != a.Config.FilenameMode {
				a.logFallbackRestore(database, key, backupTime)
			}
			if days > 0 {
				if err := a.removeOldBackups(ctx, database, key, days); err != nil {
					fmt.Fprintf(a.Err, "Retention cleanup failed: %s: %v\n", label, err)
					cleanupFailed++
				}
			}
		}
	}
	fmt.Fprintf(a.Out, "Backup summary: %d succeeded, %d failed, %d retention cleanups failed.\n", succeeded, failed, cleanupFailed)
	if err := ctx.Err(); err != nil {
		return err
	}
	if failed > 0 || cleanupFailed > 0 {
		return fmt.Errorf("backup run completed with failures")
	}
	return nil
}

func (a *App) connectionArgs() []string {
	args := []string{"--protocol=tcp", "--host=" + a.Config.Host, "--port=" + a.Config.Port, "--user=" + a.Config.User, "--default-character-set=utf8mb4"}
	if a.Config.SSLCA != "" {
		args = append(args, "--ssl-ca="+a.Config.SSLCA, "--ssl-verify-server-cert")
	}
	return args
}

func (a *App) databases(ctx context.Context, runner commandRunner) ([]string, error) {
	c := a.Config
	var names []string
	if c.BackupAll == "true" {
		fmt.Fprintln(a.Out, "Discovering databases...")
		args := append(a.connectionArgs(), "--batch", "--raw", "--skip-column-names",
			"--execute=SELECT HEX(SCHEMA_NAME) FROM information_schema.SCHEMATA WHERE BINARY IF(@@lower_case_table_names=0, SCHEMA_NAME, LOWER(SCHEMA_NAME)) NOT IN ('information_schema', 'performance_schema', 'mysql', 'sys') ORDER BY SCHEMA_NAME;")
		var output bytes.Buffer
		if err := runner.run(ctx, &output, "mariadb", args...); err != nil {
			return nil, fmt.Errorf("Could not discover databases: %w", err)
		}
		for _, line := range strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n") {
			decoded, err := hex.DecodeString(line)
			if err != nil {
				return nil, fmt.Errorf("invalid database discovery result: %w", err)
			}
			names = append(names, string(decoded))
		}
	} else if c.Databases != "" {
		names = parseDatabaseList(c.Databases)
	} else {
		names = []string{c.Database}
	}
	if err := validateDatabases(names); err != nil {
		return nil, err
	}
	var excluded []string
	if c.Exclude != "" {
		excluded = parseDatabaseList(c.Exclude)
		if err := validateDatabases(excluded); err != nil {
			return nil, err
		}
	}
	// Fold using MariaDB's own identifier character mapping, preserving the
	// spelling and ordering of the first selected database in object keys.
	keys := slices.Concat(names, excluded)
	if runner.foldDatabaseNames && len(keys) > 1 {
		for batch := range slices.Chunk(keys, 128) {
			payload, err := json.Marshal(batch)
			if err != nil {
				return nil, err
			}
			query := "SELECT HEX(LOWER(j.name)) FROM JSON_TABLE(" + sqlText(string(payload)) + ", '$[*]' COLUMNS (pos FOR ORDINALITY, name TEXT PATH '$')) j ORDER BY j.pos;\n"
			var out bytes.Buffer
			args := append(a.connectionArgs(), "--batch", "--raw", "--skip-column-names")
			if err := runner.runInput(ctx, strings.NewReader(query), &out, "mariadb", args...); err != nil {
				return nil, fmt.Errorf("normalize database identifiers: %w", err)
			}
			lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
			if len(lines) != len(batch) {
				return nil, errors.New("invalid normalized database list")
			}
			for i, line := range lines {
				b, err := hex.DecodeString(line)
				if err != nil || len(b) == 0 {
					return nil, errors.New("invalid normalized database name")
				}
				batch[i] = string(b)
			}
		}
	}
	omit := make(map[string]bool)
	for _, key := range keys[len(names):] {
		omit[key] = true
	}
	seen := make(map[string]bool)
	var selected []string
	for i, name := range names {
		key := keys[i]
		if omit[key] {
			fmt.Fprintf(a.Out, "Excluding database: %s\n", name)
		} else if !seen[key] {
			selected = append(selected, name)
			seen[key] = true
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("No databases selected for backup.")
	}
	return selected, nil
}

func (a *App) logFallbackRestore(database, key string, backupTime time.Time) {
	// Quote the literal database name for a POSIX shell, including embedded single quotes.
	selection := "BACKUP_MODE=per-database MARIADB_DATABASE='" + strings.ReplaceAll(database, "'", "'\\''") + "'"
	if a.Config.BackupMode == "combined" {
		selection = "BACKUP_MODE=combined MARIADB_DATABASE= MARIADB_DATABASES= MARIADB_DATABASES_EXCLUDE= MARIADB_BACKUP_ALL=false"
	}
	fmt.Fprintf(a.Out, "Saved fallback S3 key: %q\nRestore command: BACKUP_FILENAME_MODE=timestamp %s mariadb-backup-s3 restore %s\n", key, selection, backupTime.UTC().Format(timestampLayout))
}

func (a *App) backupDatabases(ctx context.Context, runner commandRunner, databases []string, key string, createOnly bool) error {
	label := strings.Join(databases, ", ")
	fmt.Fprintf(a.Out, "Creating backup of databases: %s...\n", label)
	opts, err := a.Config.dumpOptions()
	if err != nil {
		return err
	}
	if err := a.checkTables(ctx, runner, databases, opts); err != nil {
		return err
	}
	args := append(a.connectionArgs(), "--quick", "--routines", "--events", "--triggers", "--hex-blob", "--dump-history", "--copy-s3-tables")
	locked := false
	for _, opt := range opts {
		locked = locked || opt == "--lock-all-tables"
	}
	if !locked {
		args = append(args, "--single-transaction")
	}
	args = append(args, opts...)
	// Native CREATE DATABASE records the source defaults for single-database restores too.
	args = append(args, "--databases", "--")
	args = append(args, databases...)
	dump := pipelineCommand{name: "mariadb-dump", args: args}
	var encrypt *pipelineCommand
	if a.Config.Passphrase != "" {
		fmt.Fprintf(a.Out, "Encrypting backup of %s...\n", label)
		encrypt = &pipelineCommand{name: "gpg", args: []string{
			"--symmetric", "--batch", "--pinentry-mode", "loopback", "--output", "-",
		}}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	reader, writer := io.Pipe()
	defer reader.Close()
	// Closing the reader also unblocks os/exec's stdout copier on cancellation.
	stop := context.AfterFunc(ctx, func() { reader.CloseWithError(ctx.Err()) })
	defer stop()
	done := make(chan error, 1)
	go func() {
		err := runner.pipeline(ctx, writer, dump, encrypt)
		writer.CloseWithError(err)
		done <- err
	}()
	fmt.Fprintf(a.Out, "Uploading backup of %s...\n", label)
	uploadErr := a.Store.Upload(ctx, key, reader, createOnly)
	// An upload failure must stop both mariadb-dump and GPG and release backpressure.
	if uploadErr != nil {
		cancel()
		reader.CloseWithError(uploadErr)
	}
	if err := errors.Join(uploadErr, <-done); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Backup complete: %s\n", label)
	return nil
}

// Stage only expired candidates. No deletions occur until every page validates.
func (a *App) removeOldBackups(ctx context.Context, database, savedKey string, days int) error {
	f, err := tempfile.Open("", "mariadb-retention-*")
	if err != nil {
		return err
	}
	defer f.Close()
	buf := bufio.NewWriter(f)
	enc := json.NewEncoder(buf)
	err = a.Store.List(ctx, a.Config.timestampPrefix(database), func(obj storage.Object, serverTime time.Time) error {
		if obj.Key == savedKey || !a.Config.isTimestampBackup(database, obj.Key) {
			return nil
		}
		if serverTime.IsZero() || obj.LastModified.IsZero() {
			return errors.New("missing storage timestamp for retention")
		}
		if obj.LastModified.Before(serverTime.AddDate(0, 0, -days)) {
			if obj.ETag == "" || obj.ETag == "*" {
				return fmt.Errorf("missing specific ETag for %q", obj.Key)
			}
			return enc.Encode(obj)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = buf.Flush(); err != nil {
		return err
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dec := json.NewDecoder(f)
	batch := make([]storage.Object, 0, 1000)
	for {
		var obj storage.Object
		err := dec.Decode(&obj)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		batch = append(batch, obj)
		if len(batch) == cap(batch) {
			if err := a.Store.Delete(ctx, batch); err != nil {
				return err
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		return a.Store.Delete(ctx, batch)
	}
	return nil
}

func (a *App) Restore(ctx context.Context, args []string) error {
	c := a.Config
	label := c.Database
	if c.BackupMode == "combined" {
		label = "combined databases"
		if c.Database != "" || c.Databases != "" || c.Exclude != "" || c.BackupAll != "false" && c.BackupAll != "" {
			return fmt.Errorf("Combined restore restores every database in the backup; clear MARIADB_DATABASE, MARIADB_DATABASES, MARIADB_DATABASES_EXCLUDE and set MARIADB_BACKUP_ALL=false.")
		}
	} else {
		if c.Database == "" {
			return fmt.Errorf("Set MARIADB_DATABASE to the single database to restore.")
		}
		if err := validateDatabases([]string{c.Database}); err != nil {
			return err
		}
	}
	timestamp, versionID := "", ""
	switch len(args) {
	case 0:
	case 1:
		if c.FilenameMode != "timestamp" {
			return fmt.Errorf("Use --version-id ID to restore a previous fixed-name backup.")
		}
		if !validBackupTimestamp(args[0]) {
			return fmt.Errorf("Invalid backup timestamp.")
		}
		timestamp = args[0]
	case 2:
		if args[0] != "--version-id" || args[1] == "" || c.FilenameMode != "fixed" {
			return fmt.Errorf("--version-id ID requires BACKUP_FILENAME_MODE=fixed.")
		}
		versionID = args[1]
	default:
		return fmt.Errorf("Usage: restore.sh [timestamp | --version-id ID]")
	}
	return a.withRunner(nil, func(runner commandRunner) error {
		return a.withServerLock(ctx, runner, true, func(ctx context.Context, runner commandRunner) error {
			var key string
			switch {
			case c.FilenameMode == "fixed":
				key = c.fixedKey(c.Database)
			case timestamp != "":
				key = c.timestampPrefix(c.Database) + timestamp + c.fileType()
			default:
				fmt.Fprintf(a.Out, "Finding latest backup of %s...\n", label)
				prefix, suffix := c.timestampPrefix(c.Database), c.fileType()
				var latest time.Time
				err := a.Store.List(ctx, prefix, func(obj storage.Object, _ time.Time) error {
					if c.isTimestampBackup(c.Database, obj.Key) && strings.HasSuffix(obj.Key, suffix) {
						if obj.LastModified.After(latest) || obj.LastModified.Equal(latest) && strings.TrimSuffix(obj.Key, suffix) > strings.TrimSuffix(key, suffix) {
							latest = obj.LastModified
							key = obj.Key
						}
					}
					return nil
				})
				if err != nil {
					return fmt.Errorf("Could not list backups: %w", err)
				}
				if key == "" {
					return fmt.Errorf("No backup found for %s.", label)
				}
			}
			fmt.Fprintf(a.Out, "Fetching backup of %s from S3...\n", label)
			file, err := a.Store.Download(ctx, key, versionID)
			if err != nil {
				return err
			}
			defer file.Close()
			if c.Passphrase != "" {
				fmt.Fprintln(a.Out, "Decrypting backup...")
				plain, err := tempfile.Open("", "mariadb-restore-*")
				if err != nil {
					return err
				}
				defer plain.Close()
				if err = runner.runInput(ctx, file, plain, "gpg", "--decrypt", "--batch", "--pinentry-mode", "loopback"); err != nil {
					return err
				}
				if _, err = plain.Seek(0, io.SeekStart); err != nil {
					return err
				}
				file = plain
			}
			fmt.Fprintf(a.Out, "Restoring %s from backup...\n", label)
			info, err := file.Stat()
			if err != nil {
				return err
			}
			if info.Size() == 0 {
				return errors.New("empty SQL backup")
			}
			if err := runner.importSQL(file); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "Restore complete.")
			return nil
		})
	})
}
