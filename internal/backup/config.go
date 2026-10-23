package backup

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ajctrl/mariadb-backup-s3/internal/storage"
)

const timestampLayout = "2006-01-02T15:04:05.000000000Z"
const defaultBackupTimeout = time.Hour

var backupIDPattern = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{9}Z-[0-9a-f]{32}$`)

func validBackupID(id string) bool {
	if !backupIDPattern.MatchString(id) {
		return false
	}
	_, err := time.Parse(timestampLayout, id[:len(timestampLayout)])
	return err == nil
}

type Config struct {
	Database, Databases, BackupAll, Exclude, BackupMode  string
	Host, Port, User, Password, DumpOptions, SSLCA       string
	Prefix, Passphrase, KeepDays, FilenameMode, Schedule string
	RestoreDatabaseDefaults                              string
	BackupTimeout                                        time.Duration
	Storage                                              storage.Config
}

func LoadConfig() (Config, error) {
	return loadConfig(os.LookupEnv)
}

func loadConfig(lookup func(string) (string, bool)) (Config, error) {
	get := func(key string) string { value, _ := lookup(key); return value }
	valueOr := func(key, fallback string) string {
		if value := get(key); value != "" {
			return value
		}
		return fallback
	}
	prefix, present := lookup("S3_PREFIX")
	if !present {
		prefix = "backup"
	}
	c := Config{
		RestoreDatabaseDefaults: get("RESTORE_DATABASE_DEFAULTS"),
		Database:                get("MARIADB_DATABASE"), Databases: get("MARIADB_DATABASES"),
		BackupAll: valueOr("MARIADB_BACKUP_ALL", "false"), Exclude: get("MARIADB_DATABASES_EXCLUDE"),
		BackupMode: valueOr("BACKUP_MODE", "per-database"), SSLCA: get("MARIADB_SSL_CA"),
		Host: get("MARIADB_HOST"), Port: valueOr("MARIADB_PORT", "3306"),
		User: get("MARIADB_USER"), Password: get("MARIADB_PASSWORD"), DumpOptions: get("MARIADB_DUMP_EXTRA_OPTS"),
		Prefix: prefix, Passphrase: get("PASSPHRASE"), KeepDays: get("BACKUP_KEEP_DAYS"),
		FilenameMode: valueOr("BACKUP_FILENAME_MODE", "timestamp"), Schedule: get("SCHEDULE"),
		Storage: storage.Config{
			Bucket: get("S3_BUCKET"), Region: valueOr("S3_REGION", "us-west-1"), Endpoint: get("S3_ENDPOINT"),
			AccessKeyID: get("S3_ACCESS_KEY_ID"), SecretAccessKey: get("S3_SECRET_ACCESS_KEY"),
		},
	}
	for _, required := range []struct{ name, value string }{
		{"S3_BUCKET", c.Storage.Bucket}, {"MARIADB_USER", c.User}, {"MARIADB_PASSWORD", c.Password}, {"MARIADB_HOST", c.Host},
	} {
		if required.value == "" {
			return c, fmt.Errorf("You need to set %s.", required.name)
		}
	}
	if c.BackupMode != "per-database" && c.BackupMode != "combined" {
		return c, fmt.Errorf("BACKUP_MODE must be per-database or combined.")
	}
	if strings.ContainsRune(c.Passphrase, '\n') {
		return c, fmt.Errorf("PASSPHRASE must not contain a newline.")
	}
	port, err := strconv.Atoi(c.Port)
	if err != nil || port < 1 || port > 65535 {
		return c, fmt.Errorf("MARIADB_PORT must be an integer between 1 and 65535.")
	}
	if c.FilenameMode != "timestamp" && c.FilenameMode != "fixed" {
		return c, fmt.Errorf("BACKUP_FILENAME_MODE must be timestamp or fixed.")
	}
	partSize, err := strconv.ParseInt(valueOr("S3_UPLOAD_PART_SIZE_MB", strconv.FormatInt(storage.DefaultUploadPartSizeBytes/(1024*1024), 10)), 10, 64)
	if err != nil || partSize < 5 || partSize > 5120 {
		return c, fmt.Errorf("S3_UPLOAD_PART_SIZE_MB must be an integer between 5 and 5120 (MiB).")
	}
	c.Storage.UploadPartSizeBytes = partSize * 1024 * 1024
	c.BackupTimeout, err = time.ParseDuration(valueOr("BACKUP_TIMEOUT", defaultBackupTimeout.String()))
	if err != nil || c.BackupTimeout <= 0 {
		return c, fmt.Errorf("BACKUP_TIMEOUT must be a positive duration, such as 30m or 6h.")
	}
	return c, nil
}

func (c Config) validateBackup() (int, error) {
	if c.BackupTimeout < 0 {
		return 0, fmt.Errorf("BACKUP_TIMEOUT must be a positive duration.")
	}
	if c.BackupMode != "" && c.BackupMode != "per-database" && c.BackupMode != "combined" {
		return 0, fmt.Errorf("BACKUP_MODE must be per-database or combined.")
	}
	if _, err := c.dumpOptions(); err != nil {
		return 0, err
	}
	switch c.BackupAll {
	case "true":
		if c.Database != "" || c.Databases != "" {
			return 0, fmt.Errorf("MARIADB_BACKUP_ALL cannot be combined with MARIADB_DATABASE or MARIADB_DATABASES.")
		}
	case "false":
		if c.Database == "" && c.Databases == "" {
			return 0, fmt.Errorf("Set MARIADB_DATABASE, MARIADB_DATABASES, or MARIADB_BACKUP_ALL=true.")
		}
	default:
		return 0, fmt.Errorf("MARIADB_BACKUP_ALL must be true or false.")
	}
	if c.Exclude != "" && c.BackupAll != "true" {
		return 0, fmt.Errorf("MARIADB_DATABASES_EXCLUDE requires MARIADB_BACKUP_ALL=true.")
	}
	if c.KeepDays == "" {
		return 0, nil
	}
	if c.KeepDays[0] == '0' || strings.IndexFunc(c.KeepDays, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("BACKUP_KEEP_DAYS must be a positive integer without leading zeros.")
	}
	days, err := strconv.Atoi(c.KeepDays)
	if err != nil || days < 1 || days > 36500 {
		return 0, fmt.Errorf("BACKUP_KEEP_DAYS must be at most 36500.")
	}
	return days, nil
}

func parseDatabaseList(value string) []string {
	names := strings.Split(value, ",")
	for i := range names {
		names[i] = strings.TrimSpace(names[i])
	}
	return names
}

func (c Config) dumpOptions() ([]string, error) {
	opts := strings.Fields(c.DumpOptions)
	// Output format, connection and database selection belong to the app. Use
	// exact long options so client abbreviations cannot bypass that contract.
	for _, opt := range opts {
		name, value, hasValue := strings.Cut(opt, "=")
		switch name {
		case "--add-drop-table", "--skip-add-drop-table", "--add-drop-trigger",
			"--skip-add-locks", "--complete-insert", "--skip-extended-insert",
			"--skip-comments", "--skip-dump-date", "--no-tablespaces",
			"--order-by-primary", "--lock-all-tables", "--skip-single-transaction",
			"--skip-no-autocommit", "--no-autocommit":
			if !hasValue {
				continue
			}
		case "--ignore-table", "--ignore-table-data", "--max-allowed-packet", "--net-buffer-length":
			if hasValue && value != "" {
				continue
			}
		}
		return nil, fmt.Errorf("unsupported option in MARIADB_DUMP_EXTRA_OPTS; use the documented exact long options")
	}
	return opts, nil
}

func validateDatabases(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("Database names must be nonempty and contain no control characters.")
	}
	for _, name := range names {
		if name == "" || !utf8.ValidString(name) || strings.IndexFunc(name, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
			return fmt.Errorf("Database names must be nonempty and contain no control characters.")
		}
	}
	return nil
}

// Match jq's @uri encoding, including slashes, percent signs and UTF-8 bytes.
// PathEscape leaves several reserved characters unescaped; QueryEscape uses '+'.
func encodeComponent(value string) string {
	const hex = "0123456789ABCDEF"
	var result strings.Builder
	for i := 0; i < len(value); i++ {
		b := value[i]
		if b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-._~", rune(b)) {
			result.WriteByte(b)
		} else {
			result.WriteByte('%')
			result.WriteByte(hex[b>>4])
			result.WriteByte(hex[b&15])
		}
	}
	return result.String()
}

func (c Config) fileType() string {
	if c.Passphrase != "" {
		return ".sql.gpg"
	}
	return ".sql"
}

func (c Config) keyPrefix() string {
	prefix := strings.TrimRight(c.Prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return prefix
}

func (c Config) timestampPrefix(database string) string {
	prefix := c.keyPrefix()
	if c.BackupMode == "combined" {
		return prefix
	}
	directory := encodeComponent(database)
	if directory == "." || directory == ".." {
		directory = strings.ReplaceAll(directory, ".", "%2E")
	}
	return prefix + directory + "/"
}

func (c Config) fixedKey(database string) string {
	name := "latest"
	if c.BackupMode != "combined" {
		name = encodeComponent(database) + "_" + name
	}
	return c.keyPrefix() + name + c.fileType()
}

func (c Config) backupKey(database string, now time.Time) (string, error) {
	if c.FilenameMode == "fixed" {
		return c.fixedKey(database), nil
	}
	// Clock precision alone cannot prevent collisions across processes or when
	// the wall clock repeats. Give each object an independent 128-bit identity.
	var randomID [16]byte
	if _, err := rand.Read(randomID[:]); err != nil {
		return "", fmt.Errorf("generate backup ID: %w", err)
	}
	id := now.UTC().Format(timestampLayout) + "-" + hex.EncodeToString(randomID[:])
	return c.timestampPrefix(database) + id + c.fileType(), nil
}

func (c Config) isTimestampBackup(database, key string) bool {
	prefix := c.timestampPrefix(database)
	if !strings.HasPrefix(key, prefix) {
		return false
	}
	name := strings.TrimPrefix(key, prefix)
	name = strings.TrimSuffix(name, ".gpg")
	if !strings.HasSuffix(name, ".sql") {
		return false
	}
	return validBackupID(strings.TrimSuffix(name, ".sql"))
}
