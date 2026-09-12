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
`MARIADB_DATABASES_EXCLUDE` is a comma-separated list and only applies to
`MARIADB_BACKUP_ALL=true`. Comma-separated selectors trim surrounding whitespace.

The dump includes tables, data, views, triggers, stored routines, and events.
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
| `S3_BUCKET` | Required | Existing destination bucket |
| `S3_REGION` | `us-west-1` | AWS region |
| `S3_PREFIX` | `backup` | Key prefix; use a distinct prefix per source server and combined selection |
| `S3_ENDPOINT` | Empty | HTTP(S) endpoint for S3-compatible services; uses path-style requests |
| `S3_ACCESS_KEY_ID` / `S3_SECRET_ACCESS_KEY` | Empty | Optional credentials; AWS SDK credential providers also work |
| `S3_SESSION_TOKEN` | Empty | Optional temporary-credential token |
| `S3_UPLOAD_PART_SIZE_MB` | `8` | Multipart buffer size in MiB, from 5 through 5120 |
| `PASSPHRASE` | Empty | GPG symmetric encryption passphrase, without newlines; passed through a dedicated file descriptor |
| `SCHEDULE` | Empty | Cron expression, optionally including seconds, or a descriptor such as `@daily` |
| `BACKUP_KEEP_DAYS` | Empty | Delete expired timestamped backups after a successful backup; positive integer, at most 36500 |
| `BACKUP_FILENAME_MODE` | `timestamp` | `timestamp` or `fixed`; enable bucket versioning to preserve previous fixed-name backups |

Mount the certificate into the container when using `MARIADB_SSL_CA`. Database
credentials and restore downloads use temporary files; allow writable temporary
storage. Backup SQL streams directly into the S3 uploader without a dump file.
Restore downloads the complete object before importing it and needs enough local
space for the SQL file and, when encrypted, the encrypted download as well.

By default, `mariadb-dump` uses `--single-transaction --quick --routines --events
--triggers --hex-blob`. This provides a consistent snapshot for InnoDB tables.
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
backup/app/2026-09-12T03:00:00.sql
backup/analytics/2026-09-12T03:00:01.sql
backup-combined/2026-09-12T03:00:00.sql
```

With timestamp filenames, per-database mode writes to
`S3_PREFIX/<database>/<timestamp>.sql`; combined mode writes to
`S3_PREFIX/<timestamp>.sql`. The examples above use `S3_PREFIX=backup`
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
Before uploading, fixed mode checks bucket versioning. If the check fails (for
example, `GetBucketVersioning` returns 403 AccessDenied), it logs a warning and
continues. Without versioning, uploading overwrites the previous backup without
preserving it. If the check succeeds and versioning is not enabled, backup stops.
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

Retention uses S3 object modification times, and only deletes timestamp-shaped
SQL backup keys in the current scope. It does not remove fixed-name backups or
noncurrent object versions; configure the bucket lifecycle for those. In
fixed-name mode with versioning enabled, each upload creates a new S3 object version.

## Restore

Restore executes SQL and can overwrite existing objects. Use an empty destination
where possible. The SQL is not an atomic transaction: a failure can leave a
partially restored database, and objects absent from the dump are not removed.

Use the same `S3_PREFIX`, `BACKUP_MODE`, `BACKUP_FILENAME_MODE`, and `PASSPHRASE`
as the backup. Set database connection variables to the destination server.

In per-database mode, set `MARIADB_DATABASE` to the database whose backup you want
to restore. The destination database must already exist. Other database selection
variables do not affect per-database restore; clear them to keep the configuration
unambiguous.

```sh
docker run --rm --env-file template.env mariadb-backup-s3:11.4 mariadb-backup-s3 restore
docker run --rm --env-file template.env mariadb-backup-s3:11.4 mariadb-backup-s3 restore 2026-09-12T03:00:00
```

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

With no argument, `restore` chooses the latest matching timestamped backup or
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

Integration tests start disposable MariaDB and MinIO containers. Override
`MARIADB_TEST_IMAGE` or `BACKUP_TEST_IMAGE` to test another image. Shell wrappers
`/run.sh`, `/backup.sh`, and `/restore.sh` forward to the corresponding Go command.
