#!/bin/sh

set -eu

apk add --no-cache mariadb-client gpg gpg-agent ca-certificates

# Keep the SQL client and dump tool; omit unrelated database administration tools.
# This runs in the installation layer so removed files do not inflate the image.
rm -f \
    /usr/bin/mariadb-admin /usr/bin/mysqladmin \
    /usr/bin/mariadb-check /usr/bin/mysqlcheck \
    /usr/bin/mariadb-import /usr/bin/mysqlimport \
    /usr/bin/mariadb-show /usr/bin/mysqlshow \
    /usr/bin/mariadb-waitpid /usr/bin/mysql_waitpid \
    /usr/bin/mariadb-secure-installation /usr/bin/mysql_secure_installation \
    /usr/bin/myisam_ftdump
