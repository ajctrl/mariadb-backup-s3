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

The default image uses Alpine 3.23 and its
[MariaDB 11.4 client](https://pkgs.alpinelinux.org/package/v3.23/main/x86_64/mariadb-client).
CI tests against MariaDB 11.4. Use the same client family to dump and restore,
and test restoration before relying on backups across server versions.

```sh
docker build -t mariadb-backup-s3:11.4 .
docker run --rm --env-file template.env mariadb-backup-s3:11.4
```

Configure `template.env` with your database connection and S3 bucket first. The
default command, `run`, takes one backup when `SCHEDULE` is empty. A schedule such
as `0 3 * * *` or `@daily` keeps the process running and waits for the next tick.
`CRON_TZ=Asia/Tokyo 0 3 * * *` specifies a timezone. `backup` always runs once.

```sh
docker run --rm --env-file template.env mariadb-backup-s3:11.4 mariadb-backup-s3 backup
```

`docker compose --env-file template.env up -d --build` starts an isolated
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
excludes `information_schema`, `performance_schema`, `mysql`, and `sys`.
System-name matching follows the server's `lower_case_table_names` setting and
does not fold accents. Distinct business databases such as `SYS` (on a
case-sensitive server) or `mýsql` are included.
The bundled MariaDB 11.4 dump client treats case variants of `mysql` specially:
an ordinary business database named `MYSQL` without system log tables fails to
dump. Discovery now surfaces that client error instead of silently omitting it;
the failed dump is not published.
`MARIADB_DATABASES_EXCLUDE` is a comma-separated list and only applies to
`MARIADB_BACKUP_ALL=true`. Comma-separated selectors trim surrounding whitespace.

The dump includes tables, current and historical rows of system-versioned tables,
views, triggers, stored routines, and events. History is included by default with
`--dump-history`, requiring a MariaDB dump client version 10.11 or later (the
image uses 11.4). Historical rows can increase backup size and runtime.
Only timestamp-based system versioning is supported. Tables using `BIGINT`
`ROW START` / `ROW END` (transaction-precise history) cannot be dumped with
`--dump-history`. Before starting any dump, the app checks all selected databases
and rejects these tables with their names. Fully excluded `--ignore-table=db.table`
tables are skipped by this check; `--ignore-table-data` does not avoid the client
limitation. The app never silently discards history to work around this limit.
The check follows the server's `lower_case_table_names` setting for database
selection and the bundled MariaDB 11.4 dump client's comparison rules for
`--ignore-table`, including case-insensitive ASCII matching.
Metadata scans are limited to the selected databases, avoiding a scan of every
database's table definitions before each backup.
Users and grants are not included by default. Provision users, privileges, and
any required object definers separately on the restore server.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `MARIADB_HOST` | Required | MariaDB host, using TCP |
| `MARIADB_PORT` | `3306` | MariaDB port |
| `MARIADB_USER` | Required | Account with privileges to read all selected objects |
| `MARIADB_PASSWORD` | Required | Password; passed to clients in a temporary mode-0600 option file |
| `MARIADB_SSL_CA` | Empty | CA certificate path inside the container; enables TLS and server certificate verification |
| `MARIADB_DATABASE` | Empty | One database to back up, or the database to restore in per-database mode |
| `MARIADB_DATABASES` | Empty | Comma-separated databases to back up |
| `MARIADB_BACKUP_ALL` | `false` | Automatically discover business databases |
| `MARIADB_DATABASES_EXCLUDE` | Empty | Databases excluded from automatic discovery |
| `BACKUP_MODE` | `per-database` | `per-database` or `combined` |
| `MARIADB_DUMP_EXTRA_OPTS` | Empty | Additional dump options; see constraints below |
| `RESTORE_DATABASE_DEFAULTS` | Empty | Restore-only JSON mapping of source DB names to `character_set` and `collation`, for dumps lacking this metadata |
| `S3_BUCKET` | Required | Existing destination bucket |
| `S3_REGION` | `us-west-1` | AWS region |
| `S3_PREFIX` | `backup` | Key prefix; use a distinct prefix per source server and combined selection |
| `S3_ENDPOINT` | Empty | HTTP(S) endpoint for S3-compatible services; uses path-style requests |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | Empty | Optional credentials; AWS SDK credential providers also work |
| `S3_SESSION_TOKEN` | Empty | Optional temporary-credential token |
| `S3_UPLOAD_PART_SIZE_MB` | `8` | Multipart part size in MiB, from 5 through 5120; controls memory use and the per-object stream limit below |
| `PASSPHRASE` | Empty | GPG symmetric encryption passphrase, without newlines; passed through a dedicated file descriptor |
| `SCHEDULE` | Empty | Cron expression, optionally including seconds, or a descriptor such as `@daily` |
| `BACKUP_TIMEOUT` | `1h` | Positive duration (e.g. `30m`, `6h`) for the entire backup run, including discovery, preflight, all databases, uploads and retention; does not apply to restore |
| `BACKUP_KEEP_DAYS` | Empty | Delete expired timestamped backups after a successful backup; positive integer, at most 36500 |
| `BACKUP_FILENAME_MODE` | `timestamp` | `timestamp` or `fixed`; enable bucket versioning to preserve previous fixed-name backups |

Mount the certificate into the container when using `MARIADB_SSL_CA`. Database
credentials and restore downloads use temporary files; allow writable temporary
storage. Backup SQL streams directly into the S3 uploader without a dump file.
Restore downloads the complete object before importing it and needs enough local
space for the SQL file and, when encrypted, the encrypted download as well.

Streaming uploads have an unknown final size, so the SDK cannot automatically
increase their part size. With [S3's 10,000-part limit](https://docs.aws.amazon.com/AmazonS3/latest/userguide/qfacts.html),
the per-object stream limit is `S3_UPLOAD_PART_SIZE_MB * 10,000` MiB:

| `S3_UPLOAD_PART_SIZE_MB` | Per-object stream limit |
| --- | --- |
| `8` (default) | 78.125 GiB |
| `64` | 625 GiB |
| `128` | 1,250 GiB (about 1.22 TiB) |

This measures bytes sent to S3 after encryption when enabled, not the database's
disk usage. In combined mode it applies to the entire combined backup. Your S3
provider may impose a lower object-size limit. Choose a part size of at least
`ceil(maximum expected object size in MiB / 10,000)`, allowing headroom for growth.
For example, set `S3_UPLOAD_PART_SIZE_MB=64` for objects up to 625 GiB.
Larger parts increase memory use; the uploader buffers parts in memory with two
concurrent uploads, so allow memory for multiple parts plus process overhead.

Each backup run logs its part size and stream limit. Exceeding that limit fails
the backup and attempts to abort the multipart upload without publishing a
partial object. The error names `S3_UPLOAD_PART_SIZE_MB` so it can be increased
before retrying. A stream exactly at the limit still requires successful
completion of the dump and encryption processes before it is published.

By default, `mariadb-dump` uses `--single-transaction --quick --routines --events
--triggers --hex-blob --dump-history`. This provides a consistent snapshot for InnoDB tables.
Nontransactional engines such as MyISAM and Aria need separate coordination, and
schema changes during a dump can invalidate the snapshot or fail the backup.
In per-database mode, related databases do not share a snapshot.

Extra options are split on whitespace, without shell expansion or quote parsing.
Only the following extra options are accepted; unsupported options fail before
the dump starts:

| Purpose | Allowed options |
| --- | --- |
| SQL output | `--add-drop-table`, `--skip-add-drop-table`, `--add-drop-trigger`, `--skip-add-locks`, `--complete-insert`, `--skip-extended-insert`, `--skip-comments`, `--skip-dump-date`, `--no-tablespaces`, `--order-by-primary`, `--no-autocommit`, `--skip-no-autocommit` |
| Consistency | `--lock-all-tables`, `--skip-single-transaction` |
| Exclusions | `--ignore-table=database.table`, `--ignore-table-data=database.table` |
| Buffer sizes | `--max-allowed-packet=VALUE`, `--net-buffer-length=VALUE` |

`--lock-all-tables` replaces the default `--single-transaction` with a global read
lock for the duration of the dump and can block writes. Disabling the default
transaction without another consistency mechanism removes its snapshot guarantee.

## S3 layout and retention

```text
backup/app/2026-10-22T03:00:00.123456789Z-0123456789abcdef0123456789abcdef.sql
backup/analytics/2026-10-22T03:00:00.234567890Z-89abcdef0123456789abcdef01234567.sql
backup-combined/2026-10-22T03:00:00.123456789Z-fedcba9876543210fedcba9876543210.sql
```

With timestamp filenames, per-database mode writes to
`S3_PREFIX/<database>/<backup-id>.sql`; combined mode writes to
`S3_PREFIX/<backup-id>.sql`. Each ID contains a UTC timestamp with exactly nine
fractional digits and a random 128-bit ID encoded as 32 lowercase hexadecimal
characters. Random IDs prevent repeated clock values from producing the same
object name in normal operation. The complete S3 key is logged during upload.
Timestamp-mode uploads also use
[S3 conditional writes](https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html)
(`If-None-Match: *`) for
both single PUTs and multipart completion. If the current key already exists,
the upload fails without replacing it; conflicts or unsupported conditional
writes never fall back to an unconditional upload. S3-compatible endpoints must
honor this condition. Fixed-name mode continues to create new object versions.

The examples above use `S3_PREFIX=backup`
for per-database mode and `S3_PREFIX=backup-combined` for combined mode. Set a
distinct prefix for each source server and each combined selection. An empty
`S3_PREFIX` writes directly under `<database>/` or the bucket root, respectively.

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
Before dumping or uploading, fixed mode requires a successful bucket-versioning
check returning `Enabled`. Missing `s3:GetBucketVersioning` permission, network
errors, or a disabled/suspended status stop the backup without uploading or
running retention. The application does not change the bucket configuration.
Latest-file searches and retention only match backup files directly in the
current database directory or combined prefix.

Breaking changes: second-only timestamp filenames and restore arguments are no
longer recognized. Existing objects with those names are excluded from latest
searches and retention, and are not moved or renamed. Earlier layouts containing
`per-database/` or `combined/`, and per-database fixed names of
`<database>/latest.sql[.gpg]`, are also not automatically searched. Fixed mode now
fails when versioning cannot be verified; earlier versions warned and continued.
Dump clients without `--dump-history` support are no longer supported.
Backup runs that previously had no deadline now stop after `BACKUP_TIMEOUT`
(one hour by default). Set a longer duration for larger backups. On expiration,
the app cancels requests, kills subprocess groups and releases the backup lock
after cleanup, allowing later scheduled runs to proceed. Cleanup can take a few
additional seconds. Objects already uploaded before the timeout remain in S3;
a run spanning several databases is not atomic. Unsupported transaction-precise
history now fails during preflight, before any dump or retention cleanup starts.
Automatic discovery now includes distinct business databases that were previously
omitted because their names differed from system names only by case or accents;
this can increase the number and size of backups.

Retention uses S3 object modification times, and only deletes timestamp-shaped
SQL backup keys in the current scope. It does not remove fixed-name backups or
noncurrent object versions; configure the bucket lifecycle for those. In
fixed-name mode with versioning enabled, each upload creates a new S3 object version.

The wall clock is checked before each database backup, immediately before each
publication request (including retries), after the upload, before each retention
deletion, and at run completion. If a reading precedes the run start or a previous reading,
the run fails and stops further publication, retention, and database backups.
Equal readings are allowed. Unpublished multipart uploads are aborted. These
checks compare wall time, not Go's monotonic elapsed time.
The checks cannot atomically couple the local clock to S3: a rollback during an
already-sent request cannot undo publication or deletion. Likewise, earlier
database backups and deletions in the same run are not rolled back. A rollback
detected after publication still fails the run and prevents further retention.

## Restore

Restore executes SQL and can overwrite existing objects. Use an empty destination
where possible. The SQL is not an atomic transaction: a failure can leave a
partially restored database, and objects absent from the dump are not removed.

Restore disables the MariaDB client's automatic printing of failed SQL statements
to avoid copying row data into logs. Errors still stop the restore and remain
visible. Server error messages themselves can contain values (for example,
duplicate-key values); this option does not redact those messages.

Before executing any restore SQL, the app reads the destination server's
`max_allowed_packet` and scans the downloaded/decrypted dump with bounded memory.
It checks statement sizes, including routines with custom delimiters, against
that limit. Oversized statements or a failed preflight stop the restore before
any tables are changed. The size estimate includes comments and whitespace, so
leave some headroom; the check is for dumps produced by this tool, not arbitrary
SQL scripts. Unknown SQL-mode or client character-set assignments are rejected
rather than guessed.
This check adds one local read of the SQL file. Keep the server configuration
stable while restoring; other SQL errors can still cause partial restoration.

Routines, triggers and events can retain their original character sets in the
dump even when table data is dumped as UTF-8. Restore follows the dump's
`character_set_client` and `SET NAMES` assignments, including saved/restored
values, and synchronizes the MariaDB client's parser at those boundaries.
Mixed UTF-8, Shift-JIS/CP932, Big5, GBK, EUC and single-byte client character sets
are supported. SQL bytes are preserved without transcoding, and switching the
client parser preserves the server's result character set and connection
collation. The preflight records a small temporary insertion plan on disk;
import reads the original SQL with those instructions inserted, without making
another full SQL copy. Unknown client character sets, non-client encodings such
as UTF-16/UCS2/UTF-32, and unrecognized charset expressions fail preflight before
any restore SQL is executed.

Binary values are dumped as hexadecimal SQL: a 10 MiB BLOB produces an INSERT
larger than 20 MiB, which exceeds MariaDB's common 16 MiB server default. In that
case, configure the **destination server**, for example with
`max_allowed_packet=64M`, and retry the restore. The app does not change global
server settings. The restore client permits packets up to 1 GiB, but changing a
client option alone cannot raise the server's limit. Previously these restores
could start and fail after dropping/recreating tables; they now fail preflight.

Use the same `S3_PREFIX`, `BACKUP_MODE`, `BACKUP_FILENAME_MODE`, and `PASSPHRASE`
as the backup. Set database connection variables to the destination server.

In per-database mode, set `MARIADB_DATABASE` to the database whose backup you want
to restore. Missing destination databases are created automatically. Other database selection
variables do not affect per-database restore; clear them to keep the configuration
unambiguous.

```sh
docker run --rm --env-file template.env mariadb-backup-s3:11.4 mariadb-backup-s3 restore
docker run --rm --env-file template.env mariadb-backup-s3:11.4 mariadb-backup-s3 restore 2026-10-22T03:00:00.123456789Z-0123456789abcdef0123456789abcdef
```

Both backup modes include native `CREATE DATABASE` and `USE` statements, preserving
source database character sets, collations, and database comments. Comments from
the dump are applied to both newly created and existing databases, even when
`RESTORE_DATABASE_DEFAULTS` fills missing charset/collation metadata. A native
`CREATE DATABASE` statement without `COMMENT` restores an empty comment, clearing
any existing destination comment. External dumps without `CREATE DATABASE`
metadata leave destination comments unchanged. Before
importing any SQL, restore reads all database defaults and verifies that the destination supports the required
character sets and collations, including per-routine `ALTER DATABASE` overrides.
It then creates missing databases and applies `ALTER DATABASE` to existing ones
before creating tables, functions, procedures, triggers or events. Restore requires
`CREATE` and `ALTER` privileges on the included databases, in addition to the
privileges required by the dump. Existing database defaults are deliberately
changed to the saved values; unrelated existing table columns are not converted.
Routine-specific defaults recorded by `mariadb-dump` are preserved.

Restore also reads the destination's `lower_case_table_names` and checks database,
table, view, and sequence names before executing any restore SQL. Names that would
collapse on a case-insensitive destination cause preflight to fail, preserving the
existing destination data. Database declarations and references are resolved into
one restore plan: on a case-insensitive destination, `CREATE DATABASE app` and
`USE APP` refer to the same uniquely declared database. The same resolution applies
to `MARIADB_DATABASE` and `RESTORE_DATABASE_DEFAULTS`, including saved defaults and
comments. Distinct declarations such as `CREATE DATABASE App` and
`CREATE DATABASE app` still fail preflight; conflicting explicit settings for
aliases of the same database also fail. Without a unique `CREATE DATABASE`
declaration, differently spelled database references remain rejected conservatively.
Repeated table/view/sequence declarations with identical object spelling (such as
native view placeholders) are allowed even when their database references differ.
Non-ASCII identifiers on destinations with
`lower_case_table_names=1` or `2` are rejected conservatively because their server
case mapping cannot safely be inferred; they remain supported on case-sensitive
destinations. Unknown destination name-comparison settings also stop preflight.

External MariaDB SQL dumps are supported through the same S3 key and encryption
conventions. Dumps made with `mariadb-dump --databases` normally carry the required
metadata. For older or external dumps without complete database defaults, supply
the **source** settings explicitly, for example:

```sh
docker run --rm --env-file template.env \
  -e 'RESTORE_DATABASE_DEFAULTS={"app":{"character_set":"utf8mb4","collation":"utf8mb4_bin"}}' \
  mariadb-backup-s3:11.4 mariadb-backup-s3 restore
```

The JSON object maps database names to `character_set` and `collation`; multiple
entries can be provided for combined restores. Explicit values fill missing
metadata and must agree with any settings already in the dump. Missing settings,
unsupported collations, conflicting declarations, and unused configuration entries
fail before any restore SQL executes. The destination's defaults are never used
to guess the source's settings. Combined external dumps must select their databases
with `USE` or declare them with `CREATE DATABASE`.

Compatibility changes: new per-database dumps include their original database
name, and per-database restore rejects a different database selection in the SQL.
Old dumps lacking source defaults now require `RESTORE_DATABASE_DEFAULTS`.
External scripts using `DROP DATABASE`, `CREATE OR REPLACE DATABASE`, or database
options other than character set/collation/comment are rejected; this importer supports
MariaDB dump SQL rather than arbitrary administrative scripts. Database-setting
changes and data import are not one transaction: later permission or SQL failures
can leave a partially restored database, so use an isolated destination when
validating a backup. Avoid concurrent schema/default changes during backup or restore.

Combined dumps contain `CREATE DATABASE` and `USE` statements. Combined restore
restores every included database under its original name, creating missing
databases. It requires privileges to create and restore all included objects.
Clear all selection variables for this operation; selectors are rejected because
they cannot filter a combined SQL file:

```sh
docker run --rm --env-file template.env \
  -e BACKUP_MODE=combined -e MARIADB_DATABASE= -e MARIADB_DATABASES= \
  -e MARIADB_DATABASES_EXCLUDE= -e MARIADB_BACKUP_ALL=false \
  mariadb-backup-s3:11.4 mariadb-backup-s3 restore
```

To select a timestamped backup, pass its complete backup ID (the filename without
`.sql` or `.sql.gpg`). With no argument, `restore` chooses the latest matching
timestamped backup by its filename's UTC timestamp, using the random ID as a
deterministic tie-breaker when timestamps are identical, or
the current `<database>_latest.sql[.gpg]` object in per-database fixed mode
(`latest.sql[.gpg]` in combined fixed mode). To restore an older fixed object version:

```sh
docker run --rm --env-file template.env \
  -e BACKUP_FILENAME_MODE=fixed \
  mariadb-backup-s3:11.4 mariadb-backup-s3 restore --version-id VERSION_ID
```

## Development

```sh
go test -count=1 -race ./...
go vet ./...
docker build -t mariadb-backup-s3:integration .
go test -count=1 -tags=integration -v -timeout=15m ./tests/integration
```

CLI tests build their child executable with `-race` when the test suite uses
`-race`, so the CLI's own goroutines are checked as well as the test harness.

Integration tests start disposable MariaDB and Silo containers. The default Silo
image uses the lightweight distroless variant and is pinned by digest. Override
`MARIADB_TEST_IMAGE`, `BACKUP_TEST_IMAGE`, or `S3_TEST_IMAGE` to test another image.
The S3 image must accept `server /data` and the `MINIO_ROOT_USER` and
`MINIO_ROOT_PASSWORD` environment variables. Shell wrappers
`/run.sh`, `/backup.sh`, and `/restore.sh` forward to the corresponding Go command.
