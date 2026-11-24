# mariadb-backup-s3

Stream MariaDB SQL backups to S3 or an S3-compatible service, with optional GPG
encryption, scheduling, retention, and versioned fixed filenames. Based on
`postgres-backup-s3-master.zip`; the original attribution is in `LICENSE.txt`.

`BACKUP_MODE` controls the output independently of which databases are selected:

| Mode | Output | Snapshot |
| --- | --- | --- |
| `per-database` (default) | One SQL object per database | Each database has its own snapshot |
| `combined` | One SQL object containing every selected database | One dump invocation and InnoDB snapshot across selected databases |

## Build and run

Supported image tags: `10.11`, `11.4`, `11.8`, `12.3`.

Choose the tag matching your server series. Each image contains the SQL client and dump utility
from the matching official MariaDB image. CI tests backup and restore with
that server on both `linux/amd64` and `linux/arm64`. A plain local build and
the Compose example use `11.4`. To build another series, pass e.g.
`--build-arg MARIADB_SERIES=12.3` to `docker build`.

After a tested commit lands on `main`, CI publishes each series to GHCR
under `<series>-sha-<commit>` and promotes its series tag while that commit
is still the head of `main`. For example, pull
`ghcr.io/ajctrl/mariadb-backup-s3:11.8` for an 11.8 server.
Renovate checks daily at 03:17 UTC and updates the pinned client and test
server together within each supported series. At the same time, the workflow checks
the [MariaDB Foundation release catalog](https://mariadb.org/downloads-rest-api/)
for new stable Community LTS series. Once a numeric patch tag is available for
both Linux architectures in the official MariaDB image, the GitHub App opens
one PR for that series. Review its backup and restore results and merge the
first PR manually. Later patch and digest PRs follow the usual Renovate policy.
While the PR is open, a new patch or digest updates the bot-authored image pin
and triggers CI again. Maintainer edits to the pin take precedence.
For an existing series whose image recipe changes, CI also restores a backup
created by the base commit's image and checks the rows and stored procedure.
Closing a series proposal leaves it closed on future checks. Reopen it to resume.

Use the same client family to dump and restore,
and test restoration before relying on backups across server versions.

```sh
test -e .env || (umask 077 && cp template.env .env)
# Edit .env with your connection and storage settings.
docker build -t mariadb-backup-s3:11.4 .
docker run --rm --env-file .env mariadb-backup-s3:11.4
```

The default command, `run`, takes one backup when `SCHEDULE` is empty. A schedule such
as `0 3 * * *` or `@daily` keeps the process running and waits for the next tick.
`CRON_TZ=Asia/Tokyo 0 3 * * *` specifies a timezone. An `@every` interval must be at least one second.
`backup` always runs once.

Backup and restore hold the same MariaDB named lock for the entire job. Backup uses
one extra persistent connection. Restore executes SQL on the connection holding the lock. A second job targeting the same server fails
immediately, including from a different container, bucket, or prefix. Point all
connections at the same server instance when using a proxy. Applications writing
directly to MariaDB do not participate in this lock.

```sh
docker run --rm --env-file .env mariadb-backup-s3:11.4 mariadb-backup-s3 backup
```

`docker compose --env-file .env up -d --build` starts an isolated
development database named `app` and the backup service. The Compose database
uses development-only root credentials and requires your configured S3 bucket.

## Database selection

Use `MARIADB_DATABASE` for one database, `MARIADB_DATABASES` for a list, or
`MARIADB_BACKUP_ALL=true` for discovery. `MARIADB_DATABASES` takes precedence over
`MARIADB_DATABASE` if both are set. `MARIADB_BACKUP_ALL=true` cannot be combined
with either explicit selector. Clear unused variables when switching methods.

```env
# Back up selected databases separately.
MARIADB_DATABASE=
MARIADB_DATABASES=app,analytics
MARIADB_BACKUP_ALL=false
BACKUP_MODE=per-database
```

```env
# Back up the same selection as one SQL file.
MARIADB_DATABASE=
MARIADB_DATABASES=app,analytics
MARIADB_BACKUP_ALL=false
BACKUP_MODE=combined
S3_PREFIX=backup-combined
```

```env
# Discover business databases and back them up as one SQL file.
MARIADB_DATABASE=
MARIADB_DATABASES=
MARIADB_BACKUP_ALL=true
MARIADB_DATABASES_EXCLUDE=scratch,temp
BACKUP_MODE=combined
S3_PREFIX=backup-combined
```

Automatic discovery includes databases visible to the connection user and
excludes `information_schema`, `performance_schema`, `mysql`, and `sys` according to the server’s identifier case setting.
`MARIADB_DATABASES_EXCLUDE` is a comma-separated list and only applies to
`MARIADB_BACKUP_ALL=true`. Exclusions and duplicate names follow the server’s identifier case setting.
Comma-separated selectors trim surrounding whitespace.

The dump includes tables, data, views, triggers, stored routines, and events.
The preflight warns per database if it cannot verify schema-wide `SELECT` or
access to all routine definitions through `SHOW CREATE ROUTINE` or read access
to `mysql.proc`. Backup continues after these warnings. Tables, views or routines
hidden from the account can be absent even when the job reports success.

The preflight requires
a verifiable `TRIGGER` privilege on every included base or system-versioned table,
including tables selected with `--ignore-table-data`. MariaDB can silently hide
triggers from an underprivileged dump. The check accepts direct grants, the active role
and its nested roles, and `PUBLIC`. If the account’s privilege views do not show
a grant, it reads the active role and `PUBLIC` grants and checks the affected
tables again. An assigned role must be active on the backup connection.
Overlapping database grant patterns with conflicting `TRIGGER` privileges can
prevent verification.

Users and grants are not included by default. Provision users, privileges, and
any required object definers separately on the restore server.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `MARIADB_HOST` | Required | MariaDB host, using TCP |
| `MARIADB_PORT` | `3306` | MariaDB port |
| `MARIADB_USER` | Required | Account with privileges to read all selected objects |
| `MARIADB_PASSWORD` | Required | Password; passed to clients in a private, unlinked option file |
| `MARIADB_SSL_CA` | Empty | CA certificate path inside the container; enables TLS and server certificate verification |
| `MARIADB_DATABASE` | Empty | One database to back up, or the database to restore in per-database mode |
| `MARIADB_DATABASES` | Empty | Comma-separated databases to back up |
| `MARIADB_BACKUP_ALL` | `false` | Automatically discover business databases |
| `MARIADB_DATABASES_EXCLUDE` | Empty | Databases excluded from automatic discovery |
| `BACKUP_MODE` | `per-database` | `per-database` or `combined` |
| `MARIADB_DUMP_EXTRA_OPTS` | Empty | Additional dump options; see constraints below |
| `S3_BUCKET` | Required | Existing destination bucket |
| `S3_REGION` | `us-west-1` | AWS region |
| `S3_PREFIX` | `backup` | Key prefix; use a distinct prefix per source server and combined selection |
| `S3_ENDPOINT` | Empty | HTTPS endpoint for S3-compatible services using path-style requests |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | Empty | Optional credentials; AWS SDK credential providers also work |
| `S3_SESSION_TOKEN` | Empty | Optional temporary-credential token |
| `S3_ALLOW_HTTP` | `false` | Permit cleartext HTTP for a trusted development endpoint |
| `S3_UPLOAD_BUFFER` | `memory` | Stream with bounded part buffers, or use `disk` to stage the complete object in `TMPDIR` |
| `S3_UPLOAD_PART_SIZE_MB` | `8` | Part size in MiB, 5–64 for memory and 5–5120 for disk |
| `RESTORE_DATABASE_DEFAULTS` | Empty | Source charset and collation for a single-database dump missing those settings (JSON example below) |
| `RESTORE_LOG_ERROR_DETAILS` | `false` | Include raw MariaDB diagnostics during restore. These can contain row values. |
| `BACKUP_TIMEOUT` | `24h` | Positive Go duration for an entire backup run, including discovery, all databases and retention |
| `PASSPHRASE` | Empty | GPG symmetric encryption passphrase, without newlines; passed through a dedicated file descriptor |
| `SCHEDULE` | Empty | Cron expression, optionally including seconds, or a descriptor such as `@daily` |
| `BACKUP_KEEP_DAYS` | Empty | AWS S3 expiration age in days after a successful backup (1–36500). Custom endpoints use bucket lifecycle rules |
| `BACKUP_FILENAME_MODE` | `timestamp` | `timestamp` or `fixed`; enable bucket versioning to preserve previous fixed-name backups |
| `BACKUP_ALLOW_UNVERSIONED_OVERWRITE` | `false` | In fixed mode, `true` permits overwriting the fixed key when bucket versioning is suspended, unset, or cannot be checked |

Mount the certificate into the container when using `MARIADB_SSL_CA`. Database
credentials and GPG passphrases use mode-0600 files in `/tmp`, unlinked before
writing. The Linux clients read them through `/proc/self/fd`. Memory mode streams the dump. Its maximum object size is 10,000 times the part size
(78.125 GiB with 8 MiB parts). Budget at least four parts of RAM plus Go, SDK and
subprocess overhead. For larger backups, disk mode writes the upload stream to a
private, unlinked temporary file and sizes parts from its final length. Provide
space for the complete object, including plaintext SQL when encryption is disabled.
Restore stages its download and decrypted SQL in unlinked files before import. Provide enough local
space for the SQL file and, when encrypted, the encrypted download as well.

By default, `mariadb-dump` uses `--single-transaction --quick --routines --events
--triggers --hex-blob --dump-history --copy-s3-tables`. This provides a consistent snapshot for InnoDB tables.
Nontransactional engines such as MyISAM and Aria need separate coordination, and
schema changes during a dump can invalidate the snapshot or fail the backup.
In per-database mode, related databases do not share a snapshot.

Timestamp-based versioned tables include historical rows. Transaction-precise
history requires a separate backup method. S3-engine tables are copied through
Aria and converted back to S3 on import. Configure the restore server's S3 engine
for the intended destination. For engines that manage external data, including
CONNECT, Spider, MERGE and FEDERATED, backup requires an explicit table or data
exclusion or `--no-data-med=0`. The latter includes external rows and importing
them can write to the external data source.

The dump client can match an exclusion against multiple spellings. If the server
contains `app.Orders` and `app.orders`, for example, excluding `app.orders` would
also omit `app.Orders`. The preflight rejects these ambiguous exclusions.

Extra options are split on whitespace, without shell expansion or quote parsing.
Only the following extra options are accepted; unsupported options fail before
the dump starts:

| Purpose | Allowed options |
| --- | --- |
| SQL output | `--add-drop-table`, `--skip-add-drop-table`, `--add-drop-trigger`, `--skip-add-locks`, `--complete-insert`, `--skip-extended-insert`, `--skip-comments`, `--skip-dump-date`, `--no-tablespaces`, `--order-by-primary`, `--no-autocommit`, `--skip-no-autocommit` |
| Consistency | `--lock-all-tables`, `--skip-single-transaction` |
| External data | `--no-data-med=0` |
| Exclusions | `--ignore-table=database.table`, `--ignore-table-data=database.table` |
| Buffer sizes | `--max-allowed-packet=VALUE`, `--net-buffer-length=VALUE` |

`--lock-all-tables` replaces the default `--single-transaction` with a global read
lock for the duration of the dump and can block writes. Disabling the default
transaction without another consistency mechanism removes its snapshot guarantee.

## S3 layout and retention

```text
backup/app/2026-09-12T03:00:00.123456789.sql
backup/analytics/2026-09-12T03:00:01.234567890.sql
backup-combined/2026-09-12T03:00:00.345678901.sql
```

With timestamp filenames, per-database mode writes to
`S3_PREFIX/<database>/<timestamp>.sql`; combined mode writes to
`S3_PREFIX/<timestamp>.sql`. The examples above use `S3_PREFIX=backup`
for per-database mode and `S3_PREFIX=backup-combined` for combined mode. Set a
distinct prefix for each source server and each combined selection. An empty
`S3_PREFIX` writes directly under `<database>/` or the bucket root, respectively.
New timestamps use UTC with nine fractional digits. Restore and retention also
recognize older timestamps that end at whole seconds.

Fixed filename mode writes per-database backups to
`S3_PREFIX/<database>_latest.sql` and combined backups to `S3_PREFIX/latest.sql`.
With an empty `S3_PREFIX`, both are stored directly in the bucket root.

```text
backup/app_latest.sql
backup/analytics_latest.sql
backup-combined/latest.sql
```

Database names in paths and filenames are URL-encoded. Encrypted backups end in
`.sql.gpg`.
Fixed mode checks bucket versioning once per run. For an enabled bucket it uses
fixed keys. If versioning is suspended, unset, or the check fails (for example,
with `AccessDenied`), it warns and writes timestamped backups for that run.
Each successful fallback upload logs the S3 key and a restore command to run
with the same storage and encryption settings and the intended destination
database connection. A fixed-name restore still reads the fixed key, which may
be older or absent.

Deployments that previously updated fixed keys after a versioning check error
now fall back by default. To keep writing fixed keys, enable bucket versioning
and grant the backup key access to `GetBucketVersioning`, or explicitly set
`BACKUP_ALLOW_UNVERSIONED_OVERWRITE=true` to accept possible loss of the previous
backup. The override also applies when the bucket reports suspended or unset
versioning. Timestamp mode always uses timestamped keys.
Latest-file searches and retention only match backup files directly in the
current database directory or combined prefix.

Earlier versions inserted `per-database/` or `combined/` after `S3_PREFIX`.
Existing objects are not moved or automatically searched. For timestamp backups
and combined fixed backups, append `/per-database` or `/combined` to the original
`S3_PREFIX` to restore or manage those backups
(or use `per-database` or `combined` if the original prefix was empty).
Older per-database fixed backups used `<database>/latest.sql[.gpg]`. Restore those
with the previous application version, including when selecting an old object
version; this version uses `<database>_latest.sql[.gpg]` and does not move old objects.

Timestamped uploads use conditional creation. Retention measures age using the
storage service's Date header and object modification times. It stages expired
keys in temporary storage, skips the key just saved, and issues conditional
multi-object deletes in batches of up to 1,000 after validating every list page.
Application-managed retention runs on the default AWS S3 endpoint. Use immutable
timestamped keys in this prefix because ETags can repeat for identical content.
On custom endpoints, including endpoints set via the AWS SDK environment or
profile, the application skips deletion and logs a reminder if `BACKUP_KEEP_DAYS`
is set. For example, to keep 30 days on MinIO, configure a 30-day expiration rule
on a dedicated timestamped-backup prefix in MinIO. Until the rule is configured,
old objects accumulate. For AWS application-managed retention, grant
`s3:GetObject` and `s3:DeleteObject` for conditional deletion. Retention only deletes timestamp-shaped
SQL backup keys in the current scope. It does not remove fixed-name backups or
noncurrent object versions; configure the bucket lifecycle for those. In
fixed-name mode with versioning enabled, each upload creates a new S3 object version.

## Restore

Restore executes SQL and can overwrite existing objects. Use an empty destination
where possible. Restore reads a single full GET, verifies its byte count and
available supported S3 checksum, and rejects empty SQL before import. An explicit
`--version-id` requires the same version ID in the storage response.
Before executing the staged SQL, restore checks the statement sizes against the
session's `max_allowed_packet`. This includes hexadecimal expansion of binary data.
For dumps larger than that limit, the preflight reads the SQL before import and
requires a recognizable delimiter and SQL mode when measuring statements.
Restore discards SQL result rows. Its default error logs show the MariaDB error
number, SQLSTATE and input line when available.
A SQL error during import can leave a
partially restored database, and objects absent from the dump are not removed.

Use the same `S3_PREFIX`, `BACKUP_MODE`, `BACKUP_FILENAME_MODE`, and `PASSPHRASE`
as the backup. Set database connection variables to the destination server.

In per-database mode, set `MARIADB_DATABASE` to the database to restore. New
backups include the source database's native `CREATE DATABASE` metadata.
Restore creates a missing database with the saved character set and collation.
The restore account needs `CREATE` for that operation. For an existing database,
restore compares the source settings and reports any difference before importing
SQL. It leaves existing database defaults and comments in place. A dump that
requires a different database default for a stored routine can be restored into
a new database.

If an older dump lacks some or all source settings, an existing destination can
be restored with its current defaults. The log warns that source defaults could not
be fully verified. To create a missing database from such a dump, supply the original
settings, for example in your environment file:

```text
RESTORE_DATABASE_DEFAULTS={"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}
```

```sh
docker run --rm --env-file .env mariadb-backup-s3:11.4 mariadb-backup-s3 restore
docker run --rm --env-file .env mariadb-backup-s3:11.4 mariadb-backup-s3 restore 2026-09-12T03:00:00.123456789
```

Combined dumps contain `CREATE DATABASE` and `USE` statements. Combined restore
restores every included database under its original name, creating missing
databases. It requires privileges to create and restore all included objects.
Clear all selection variables for this operation; selectors are rejected because
they cannot filter a combined SQL file:

```sh
docker run --rm --env-file .env \
  -e BACKUP_MODE=combined -e MARIADB_DATABASE= -e MARIADB_DATABASES= \
  -e MARIADB_DATABASES_EXCLUDE= -e MARIADB_BACKUP_ALL=false \
  mariadb-backup-s3:11.4 mariadb-backup-s3 restore
```

With no argument in timestamp mode, `restore` chooses the matching object with the
latest S3 `LastModified`. If storage times tie, the later timestamp in the filename
wins. Copying an old backup to S3 can therefore make it the default restore choice.
The upload buffer, part size, backup timeout, and overwrite override only apply to
backups. In fixed mode, restore selects the current `<database>_latest.sql[.gpg]` object in per-database fixed mode
(`latest.sql[.gpg]` in combined fixed mode). To restore an older fixed object version:

```sh
docker run --rm --env-file .env \
  -e BACKUP_FILENAME_MODE=fixed \
  mariadb-backup-s3:11.4 mariadb-backup-s3 restore --version-id VERSION_ID
```

## Development

```sh
CLI_TEST_RACE=1 GORACE=atexit_sleep_ms=0 go test -count=1 -race ./...
go vet ./...
docker build -t mariadb-backup-s3:integration .
go test -count=1 -tags=integration -v -timeout=15m ./tests/integration
```

Integration tests start disposable MariaDB and MinIO containers. Override
`MARIADB_TEST_IMAGE`, `S3_TEST_IMAGE`, or `BACKUP_TEST_IMAGE` to test another image. Shell wrappers
`/run.sh`, `/backup.sh`, and `/restore.sh` forward to the corresponding Go command.
