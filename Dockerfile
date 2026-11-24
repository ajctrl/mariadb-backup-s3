ARG ALPINE_VERSION=3.23
ARG GO_VERSION=1.26
ARG MARIADB_SERIES=11.4

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags='-s -w' -o /out/mariadb-backup-s3 ./cmd/mariadb-backup-s3

FROM mariadb:10.11.19@sha256:e5fe75b2b9de0eb9f6f9a4d9c97272fb3a7480e389ccd487a786bab2a12cc0d7 AS client-10.11
FROM mariadb:11.4.13@sha256:a8992b5e5404d11e03c54a31d0c1bdea3fd116d283aa6d1a627551f29045309b AS client-11.4
FROM mariadb:11.8.9@sha256:e1d5495f445654134f226f55a41ce0da227116f6f27f283101f405c794d841fc AS client-11.8
FROM mariadb:12.3.3@sha256:4aaf88b3106382c8c18e49f0cec60267be618fc22f858c4503afb2a2555afed3 AS client-12.3

FROM client-${MARIADB_SERIES} AS runtime-files
COPY src/install.sh /install.sh
RUN sh /install.sh && rm /install.sh
# Copy the client filesystem without retaining server layers or the database volume.
FROM scratch
COPY --from=runtime-files / /
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
ENV S3_ACCESS_KEY_ID=''
ENV S3_SECRET_ACCESS_KEY=''
ENV S3_BUCKET=''
ENV S3_REGION='us-west-1'
ENV S3_PREFIX='backup'
ENV S3_ENDPOINT=''
ENV S3_UPLOAD_PART_SIZE_MB=8
ENV SCHEDULE=''
ENV PASSPHRASE=''
ENV BACKUP_KEEP_DAYS=''
ENV BACKUP_FILENAME_MODE='timestamp'
ENV BACKUP_ALLOW_UNVERSIONED_OVERWRITE='false'

COPY --chmod=755 src/run.sh src/backup.sh src/restore.sh /
COPY LICENSE LICENSE.txt /usr/share/licenses/mariadb-backup-s3/

ENV HOME=/home/mariadb-backup
USER 10001:10001
CMD ["mariadb-backup-s3", "run"]
