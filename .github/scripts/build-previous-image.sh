#!/usr/bin/env bash
# Build the PR base's backup tool when this series' image inputs change.
set -euo pipefail
previous=$(mktemp -d)
trap 'rm -rf "$previous"' EXIT
git archive "$BASE_SHA" | tar -x -C "$previous"
stage=client-$MARIADB_SERIES
server=$(awk -v s="$stage" '$1=="FROM" && $4==s {print $2}' "$previous/Dockerfile")
if [[ -z "$server" ]]; then
  # The pre-matrix main branch packaged only Alpine's MariaDB 11.4 client.
  if [[ "$MARIADB_SERIES" == 11.4 ]] && grep -Fxq 'FROM alpine:${ALPINE_VERSION}' "$previous/Dockerfile"; then
    server=mariadb:11.4
  else
    echo "No previous backup image for new series $MARIADB_SERIES"
    exit 0
  fi
fi

# Ignore changes confined to the other series' pinned images.
series_recipe() {
  awk -v s="$stage" '$1!="FROM" || $2 !~ /^mariadb:/ || $4==s' "$1"
}
if cmp -s <(series_recipe "$previous/Dockerfile") <(series_recipe Dockerfile) &&
   git diff --quiet "$BASE_SHA" -- .dockerignore go.mod go.sum cmd internal src ':(exclude,glob)**/*_test.go'; then
  exit 0
fi
image=mariadb-backup-s3:previous
docker buildx build --load --provenance=false --build-arg "MARIADB_SERIES=$MARIADB_SERIES" -t "$image" "$previous"
docker run --rm "$image" mariadb-dump --version
docker pull "$server"
{
  echo "PREVIOUS_BACKUP_IMAGE=$image"
  echo "PREVIOUS_MARIADB_IMAGE=$server"
} >> "$GITHUB_ENV"
