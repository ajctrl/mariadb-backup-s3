#!/usr/bin/env bash
set -euo pipefail
script=$(realpath .github/scripts/build-previous-image.sh)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
cd "$fixture"
git init -q
mkdir -p bin cmd/backup internal/storage src
cat > Dockerfile <<'EOF'
FROM mariadb:11.4.1 AS client-11.4
FROM mariadb:11.8.1 AS client-11.8
EOF
touch go.mod go.sum .dockerignore cmd/backup/main.go internal/storage/s3.go internal/storage/s3_test.go src/install.sh
git add Dockerfile .dockerignore go.mod go.sum cmd internal src
BASE_SHA=$(git write-tree)
export BASE_SHA MARIADB_SERIES=11.4 GITHUB_ENV="$fixture/env"

# Exercise the production change detection without starting a Docker daemon.
cat > bin/docker <<'EOF'
#!/bin/sh
echo "$*" >> "$GITHUB_ENV.calls"
EOF
chmod +x bin/docker
export PATH="$fixture/bin:$PATH"
check() {
  local expected=$1
  : > "$GITHUB_ENV"
  : > "$GITHUB_ENV.calls"
  bash "$script"
  if [[ "$expected" == build ]]; then
    grep -Fqx 'PREVIOUS_BACKUP_IMAGE=mariadb-backup-s3:previous' "$GITHUB_ENV"
    grep -Fqx 'PREVIOUS_MARIADB_IMAGE=mariadb:11.4.1' "$GITHUB_ENV"
    grep -Fq 'buildx build ' "$GITHUB_ENV.calls"
  else
    test ! -s "$GITHUB_ENV"
    test ! -s "$GITHUB_ENV.calls"
  fi
}

check skip
for path in go.mod go.sum .dockerignore cmd/backup/main.go internal/storage/s3.go src/install.sh; do
  echo changed > "$path"
  check build
  : > "$path"
done
echo changed > internal/storage/s3_test.go
sed -i 's/11.8.1/11.8.2/' Dockerfile
check skip
sed -i 's/11.4.1/11.4.2/' Dockerfile
check build
MARIADB_SERIES=99.3 check skip
