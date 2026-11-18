#!/bin/sh
# Nightly logical backup, run by the `backup` service in docker-compose.prod.yml.
# Writes /backups/tautan-<UTC timestamp>.sql.gz and keeps the newest $KEEP dumps.
# Copy ./backups off the box (rclone to R2): see docs/DEPLOY.md "Backups".
set -eu
KEEP="${KEEP:-14}"
while true; do
  f="/backups/tautan-$(date -u +%Y%m%dT%H%M%SZ).sql.gz"
  if pg_dump -h db -U tautan -d tautan --no-owner | gzip > "$f.tmp"; then
    mv "$f.tmp" "$f"
    echo "backup ok: $f"
  else
    echo "backup FAILED" >&2
  fi
  # Prune: everything older than the newest $KEEP dumps.
  ls -1t /backups/tautan-*.sql.gz 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
    unlink "$old"
  done
  sleep 86400
done
