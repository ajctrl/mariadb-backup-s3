ARG ALPINE_VERSION='3.23'
ARG GO_VERSION='1.26'

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/mariadb-backup-s3 ./cmd/mariadb-backup-s3

FROM alpine:${ALPINE_VERSION}
COPY src/install.sh /install.sh
RUN sh /install.sh && rm /install.sh
COPY --from=build /out/mariadb-backup-s3 /usr/local/bin/mariadb-backup-s3

ENV MARIADB_DATABASE=''
ENV MARIADB_DATABASES=''
ENV MARIADB_BACKUP_ALL='false'
ENV MARIADB_DATABASES_EXCLUDE=''
ENV MARIADB_HOST=''
ENV MARIADB_PORT=3306
ENV MARIADB_USER=''
ENV MARIADB_PASSWORD=''
ENV MARIADB_DUMP_EXTRA_OPTS=''
ENV MARIADB_SSL_CA=''
ENV BACKUP_MODE='per-database'
ENV RESTORE_DATABASE_DEFAULTS=''
ENV S3_ACCESS_KEY_ID=''
ENV S3_SECRET_ACCESS_KEY=''
ENV S3_BUCKET=''
ENV S3_REGION='us-west-1'
ENV S3_PREFIX='backup'
ENV S3_ENDPOINT=''
ENV S3_UPLOAD_PART_SIZE_MB=8
ENV SCHEDULE=''
ENV BACKUP_TIMEOUT='1h'
ENV PASSPHRASE=''
ENV BACKUP_KEEP_DAYS=''
ENV BACKUP_FILENAME_MODE='timestamp'

COPY --chmod=755 src/run.sh src/backup.sh src/restore.sh /
COPY LICENSE LICENSE.txt /usr/share/licenses/mariadb-backup-s3/

CMD ["mariadb-backup-s3", "run"]
