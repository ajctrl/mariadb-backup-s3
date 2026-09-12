#!/bin/sh

exec mariadb-backup-s3 run "$@"
