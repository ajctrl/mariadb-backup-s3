package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/ajctrl/mariadb-backup-s3/internal/storage"
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
	return &App{Config: c, Store: store, Out: out, Err: stderr, now: time.Now, lockPath: "/tmp/mariadb-backup-s3.lock"}
}

func scheduleParser() cron.Parser {
	return cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
}

func (a *App) Run(ctx context.Context) error {
	if a.Config.Schedule == "" {
		return a.Backup(ctx)
	}
	if _, err := a.Config.validateBackup(); err != nil {
		return err
	}
	schedule, err := scheduleParser().Parse(a.Config.Schedule)
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
	// Credentials are small and independent of TMPDIR; backup SQL never touches disk.
	dir, err := os.MkdirTemp("/tmp", "mariadb-backup-s3-auth-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "client.cnf")
	if err := os.WriteFile(path, []byte("[client]\npassword="+quoteOption(a.Config.Password)+"\n"), 0600); err != nil {
		return err
	}
	runner := commandRunner{defaultsFile: path, stderr: a.Err, lock: lock}
	if a.Config.Passphrase != "" {
		runner.passphraseFile = filepath.Join(dir, "gpg-passphrase")
		if err := os.WriteFile(runner.passphraseFile, []byte(a.Config.Passphrase), 0600); err != nil {
			return err
		}
	}
	return action(runner)
}

func quoteOption(value string) string {
	escape := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t", "\b", "\\b")
	return "\"" + escape.Replace(value) + "\""
}

func withWorkspace(action func(string) error) (err error) {
	dir, err := os.MkdirTemp("", "mariadb-backup-s3-")
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary backup files: %w", cleanupErr))
		}
	}()
	return action(dir)
}

func (a *App) Backup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
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
	// The shell implementation captured epoch seconds at run start. Preserve
	// that boundary so objects at the cutoff second are still retained.
	cutoff := a.now().UTC().Truncate(time.Second).Add(-time.Duration(days) * 24 * time.Hour)
	if a.Config.FilenameMode == "fixed" {
		enabled, err := a.Store.VersioningEnabled(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(a.Err, "Warning: Could not verify S3 bucket versioning: %v. Continuing with fixed filenames; if versioning is disabled, uploading will overwrite the previous backup without preserving it.\n", err)
		} else if !enabled {
			return fmt.Errorf("Fixed filenames require S3 bucket versioning to be Enabled.")
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
		if err := a.backupDatabases(ctx, runner, group); err != nil {
			fmt.Fprintf(a.Err, "Backup failed: %s: %v\n", label, err)
			failed++
		} else {
			succeeded++
			if days > 0 {
				if err := a.removeOldBackups(ctx, database, cutoff); err != nil {
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
			"--execute=SELECT HEX(SCHEMA_NAME) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME NOT IN ('information_schema', 'performance_schema', 'mysql', 'sys') ORDER BY SCHEMA_NAME;")
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
	excluded := make(map[string]bool)
	if c.Exclude != "" {
		list := parseDatabaseList(c.Exclude)
		if err := validateDatabases(list); err != nil {
			return nil, err
		}
		for _, name := range list {
			excluded[name] = true
		}
	}
	seen := make(map[string]bool)
	var selected []string
	for _, name := range names {
		if excluded[name] {
			fmt.Fprintf(a.Out, "Excluding database: %s\n", name)
		} else if !seen[name] {
			selected = append(selected, name)
			seen[name] = true
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("No databases selected for backup.")
	}
	return selected, nil
}

func (a *App) backupDatabases(ctx context.Context, runner commandRunner, databases []string) error {
	database := databases[0]
	key := a.Config.backupKey(database, a.now())
	label := strings.Join(databases, ", ")
	fmt.Fprintf(a.Out, "Creating backup of databases: %s...\n", label)
	opts, err := a.Config.dumpOptions()
	if err != nil {
		return err
	}
	args := append(a.connectionArgs(), "--quick", "--routines", "--events", "--triggers", "--hex-blob")
	locked := false
	for _, opt := range opts {
		locked = locked || opt == "--lock-all-tables"
	}
	if !locked {
		args = append(args, "--single-transaction")
	}
	args = append(args, opts...)
	if a.Config.BackupMode == "combined" {
		args = append(args, "--databases")
	}
	args = append(args, "--")
	args = append(args, databases...)
	commands := []pipelineCommand{{name: "mariadb-dump", args: args}}
	if a.Config.Passphrase != "" {
		fmt.Fprintf(a.Out, "Encrypting backup of %s...\n", label)
		commands = append(commands, pipelineCommand{name: "gpg", args: []string{
			"--symmetric", "--batch", "--pinentry-mode", "loopback", "--output", "-",
		}})
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
		err := runner.pipeline(ctx, writer, commands...)
		writer.CloseWithError(err)
		done <- err
	}()
	fmt.Fprintf(a.Out, "Uploading backup of %s...\n", label)
	uploadErr := a.Store.Upload(ctx, key, reader)
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

func (a *App) timestampBackups(ctx context.Context, database string) ([]storage.Object, error) {
	objects, err := a.Store.List(ctx, a.Config.timestampPrefix(database))
	if err != nil {
		return nil, err
	}
	var backups []storage.Object
	for _, object := range objects {
		if a.Config.isTimestampBackup(database, object.Key) {
			backups = append(backups, object)
		}
	}
	return backups, nil
}

func (a *App) removeOldBackups(ctx context.Context, database string, cutoff time.Time) error {
	objects, err := a.timestampBackups(ctx, database)
	if err != nil {
		return err
	}
	for _, object := range objects {
		// Missing timestamps must never turn an incomplete response into deletion.
		if object.LastModified.IsZero() {
			return fmt.Errorf("missing LastModified for %q", object.Key)
		}
	}
	for _, object := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if object.LastModified.Before(cutoff) {
			if err := a.Store.Delete(ctx, object.Key); err != nil {
				return err
			}
		}
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
		if !timestampPattern.MatchString(args[0]) {
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
		return withWorkspace(func(dir string) error {
			var key string
			switch {
			case c.FilenameMode == "fixed":
				key = c.fixedKey(c.Database)
			case timestamp != "":
				key = c.timestampPrefix(c.Database) + timestamp + c.fileType()
			default:
				fmt.Fprintf(a.Out, "Finding latest backup of %s...\n", label)
				objects, err := a.timestampBackups(ctx, c.Database)
				if err != nil {
					return fmt.Errorf("Could not list backups: %w", err)
				}
				for _, object := range objects {
					if strings.HasSuffix(object.Key, c.fileType()) && object.Key > key {
						key = object.Key
					}
				}
				if key == "" {
					return fmt.Errorf("No backup found for %s.", label)
				}
			}
			fmt.Fprintf(a.Out, "Fetching backup of %s from S3...\n", label)
			if err := a.Store.Download(ctx, key, versionID, filepath.Join(dir, "db"+c.fileType())); err != nil {
				return err
			}
			if c.Passphrase != "" {
				fmt.Fprintln(a.Out, "Decrypting backup...")
				file, err := os.OpenFile(filepath.Join(dir, "db.sql"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
				if err != nil {
					return err
				}
				err = runner.run(ctx, file, "gpg", "--decrypt", "--batch", "--pinentry-mode", "loopback", filepath.Join(dir, "db.sql.gpg"))
				err = errors.Join(err, file.Close())
				if err != nil {
					return err
				}
			}
			fmt.Fprintf(a.Out, "Restoring %s from backup...\n", label)
			file, err := os.Open(filepath.Join(dir, "db.sql"))
			if err != nil {
				return err
			}
			defer file.Close()
			args := append(a.connectionArgs(), "--batch", "--binary-mode")
			if c.BackupMode != "combined" {
				args = append(args, "--database="+c.Database)
			}
			if err := runner.runInput(ctx, file, a.Out, "mariadb", args...); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "Restore complete.")
			return nil
		})
	})
}
