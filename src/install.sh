#!/bin/sh
set -eu

# Keep the client, dump tool, authentication plugins and their shared libraries.
# Purge the server before copying this filesystem into the final stage.
export DEBIAN_FRONTEND=noninteractive
apt-mark manual mariadb-client mariadb-client-core
apt-get update
apt-get install -y --no-install-recommends ca-certificates gpg gpg-agent
apt-get purge -y --auto-remove mariadb-server mariadb-server-core
rm -rf /var/lib/apt/lists/* /var/lib/mysql /docker-entrypoint-initdb.d
rm -f /usr/local/bin/docker-entrypoint.sh /usr/local/bin/healthcheck.sh
groupadd --gid 10001 mariadb-backup
useradd --uid 10001 --gid 10001 --create-home --shell /usr/sbin/nologin mariadb-backup
mariadb --version
mariadb-dump --version
gpg --version
test ! -e /usr/sbin/mariadbd
