package backup

import (
	"strings"
	"testing"
	"time"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	base := map[string]string{"S3_BUCKET": "bucket", "MARIADB_USER": "user", "MARIADB_PASSWORD": "secret", "MARIADB_HOST": "db"}
	load := func(extra map[string]string) (Config, error) {
		return loadConfig(func(key string) (string, bool) {
			if value, ok := extra[key]; ok {
				return value, true
			}
			value, ok := base[key]
			return value, ok
		})
	}
	c, err := load(nil)
	if err != nil || c.Prefix != "backup" || c.Port != "3306" || c.BackupMode != "per-database" || c.FilenameMode != "timestamp" || c.Storage.Region != "us-west-1" {
		t.Fatalf("default configuration: %#v, %v", c, err)
	}
	c, err = load(map[string]string{"S3_PREFIX": "", "MARIADB_PORT": "3307", "BACKUP_MODE": "combined"})
	if err != nil || c.Prefix != "" || c.Port != "3307" || c.BackupMode != "combined" {
		t.Fatalf("default overrides: %#v, %v", c, err)
	}
	for _, key := range []string{"S3_BUCKET", "MARIADB_USER", "MARIADB_PASSWORD", "MARIADB_HOST"} {
		if _, err := load(map[string]string{key: ""}); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("empty %s: %v", key, err)
		}
	}
	if _, err := load(map[string]string{"BACKUP_FILENAME_MODE": "wrong"}); err == nil {
		t.Fatal("accepted unknown filename mode")
	}
	if _, err := load(map[string]string{"BACKUP_MODE": "wrong"}); err == nil {
		t.Fatal("accepted unknown backup mode")
	}
	for _, port := range []string{"0", "65536", "-1", "abc"} {
		if _, err := load(map[string]string{"MARIADB_PORT": port}); err == nil {
			t.Errorf("accepted invalid port %q", port)
		}
	}
}

func TestSelectionAndRetentionValidation(t *testing.T) {
	valid := Config{Database: "app", BackupAll: "false"}
	for _, days := range []string{"", "1", "7", "36500"} {
		c := valid
		c.KeepDays = days
		if _, err := c.validateBackup(); err != nil {
			t.Errorf("valid retention %q: %v", days, err)
		}
	}
	for _, days := range []string{"0", "01", "-1", "1.0", "+1", " 1", "1\n", "abc", "36501", "999999999999999999999", "１２"} {
		c := valid
		c.KeepDays = days
		if _, err := c.validateBackup(); err == nil {
			t.Errorf("accepted invalid retention %q", days)
		}
	}
	for _, c := range []Config{
		{BackupAll: "false"}, {BackupAll: "True"}, {Database: "app", BackupAll: "true"},
		{Databases: "app,billing", BackupAll: "true"}, {Database: "app", BackupAll: "false", Exclude: "scratch"},
	} {
		if _, err := c.validateBackup(); err == nil {
			t.Errorf("accepted invalid selection %#v", c)
		}
	}
}

func TestNamesAndKeysSeparateBackupModes(t *testing.T) {
	for name, encoded := range map[string]string{
		"a/b": "a%2Fb", "a%2Fb": "a%252Fb", "a'b": "a%27b", "with space": "with%20space",
		"host=other": "host%3Dother", "$(oops)*": "%24%28oops%29%2A", "日本語": "%E6%97%A5%E6%9C%AC%E8%AA%9E",
	} {
		if got := encodeComponent(name); got != encoded {
			t.Errorf("encodeComponent(%q) = %q", name, got)
		}
	}
	for _, tc := range []struct{ prefix, timestampPrefix, fixedPrefix string }{
		{"backup", "backup/", "backup/"}, {"backup/", "backup/", "backup/"},
		{"backup//", "backup/", "backup/"}, {"", "", ""}, {"/", "", ""},
	} {
		c := Config{Prefix: tc.prefix, FilenameMode: "timestamp"}
		date := time.Date(2026, 9, 12, 12, 30, 0, 123456789, time.FixedZone("plus2", 2*3600))
		if got := c.backupKey("app", date); got != tc.timestampPrefix+"app/2026-09-12T10:30:00.123456789.sql" {
			t.Errorf("timestamp prefix %q: %q", tc.prefix, got)
		}
		c.FilenameMode, c.Passphrase = "fixed", "secret"
		for name, directory := range map[string]string{"app": "app", "a/b": "a%2Fb", ".": ".", "..": ".."} {
			if got := c.backupKey(name, date); got != tc.fixedPrefix+directory+"_latest.sql.gpg" {
				t.Errorf("fixed key %q/%q: %q", tc.prefix, name, got)
			}
		}
		c.BackupMode = "combined"
		if got := c.backupKey("app", date); got != tc.fixedPrefix+"latest.sql.gpg" {
			t.Errorf("combined fixed key: %q", got)
		}
		c.FilenameMode = "timestamp"
		if got := c.backupKey("app", date); got != tc.timestampPrefix+"2026-09-12T10:30:00.123456789.sql.gpg" {
			t.Errorf("combined timestamp key: %q", got)
		}
	}
	c := Config{Prefix: "backup", FilenameMode: "timestamp"}
	first := time.Date(2026, 9, 12, 10, 30, 0, 1, time.UTC)
	second := first.Add(time.Nanosecond)
	if firstKey, secondKey := c.backupKey("app", first), c.backupKey("app", second); firstKey == secondKey || firstKey >= secondKey {
		t.Errorf("sub-second timestamp keys are not unique and sortable: %q, %q", firstKey, secondKey)
	}
	for _, names := range [][]string{nil, {""}, {"app", ""}, {"a\nb"}, {"a\x00b"}, {"a\x7fb"}, {string([]byte{0xff})}} {
		if err := validateDatabases(names); err == nil {
			t.Errorf("accepted invalid names %q", names)
		}
	}
}

func TestDumpOptionsPreserveOutputAndSelection(t *testing.T) {
	for _, value := range []string{"--force", "--help", "--result-file=/tmp/out", "--res=/tmp/out", "--xml", "--tab=/tmp/out", "--databases", "--all-databases", "--host=elsewhere", "--password=secret", "app", "--", "--ignore-table="} {
		c := Config{Database: "app", BackupAll: "false", DumpOptions: value}
		if _, err := c.validateBackup(); err == nil {
			t.Errorf("accepted unsafe option %q", value)
		}
	}
	for _, value := range []string{"", "--lock-all-tables", "--ignore-table=app.logs --max-allowed-packet=64M", "--skip-comments --no-tablespaces"} {
		if _, err := (Config{DumpOptions: value}).dumpOptions(); err != nil {
			t.Errorf("rejected valid options %q: %v", value, err)
		}
	}
}

func TestScheduleCompatibility(t *testing.T) {
	for _, spec := range []string{"@weekly", "@daily", "@every 1h", "0 2 * * *", "0 0 2 * * *", "CRON_TZ=Asia/Tokyo 0 2 * * *"} {
		schedule, err := scheduleParser().Parse(spec)
		if err != nil || schedule.Next(time.Now()).IsZero() {
			t.Errorf("invalid schedule %q: %v", spec, err)
		}
	}
	for _, spec := range []string{"", "garbage", "* *", "61 * * * * *"} {
		if _, err := scheduleParser().Parse(spec); err == nil {
			t.Errorf("accepted invalid schedule %q", spec)
		}
	}
}

func TestUploadPartSizeConfiguration(t *testing.T) {
	for _, value := range []string{"", "5", "8", "64", "5120", "4", "5121", "-1", "1.5", "huge"} {
		t.Run(value, func(t *testing.T) {
			env := map[string]string{"S3_BUCKET": "bucket", "MARIADB_USER": "user", "MARIADB_PASSWORD": "secret", "MARIADB_HOST": "db", "S3_UPLOAD_PART_SIZE_MB": value}
			c, err := loadConfig(func(key string) (string, bool) { v, ok := env[key]; return v, ok })
			valid := value == "" || value == "5" || value == "8" || value == "64" || value == "5120"
			if (err == nil) != valid {
				t.Fatalf("part size %q: %v", value, err)
			}
			if value == "64" && c.Storage.UploadPartSizeBytes != 64*1024*1024 {
				t.Fatalf("part size was not converted to bytes: %d", c.Storage.UploadPartSizeBytes)
			}
		})
	}
}
