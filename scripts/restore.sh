#!/usr/bin/env bash
# =============================================================================
# PersonalS3 restore — bring system back from a backup directory.
#
# DESTRUCTIVE: wipes the current postgres DB and storage tree, then replaces
# them with the contents of the chosen backup. Prompts for explicit
# confirmation before doing anything.
#
# Usage:
#   scripts/restore.sh <backup_dir>           # interactive
#   scripts/restore.sh --yes <backup_dir>     # skip the confirmation
#
# Example:
#   scripts/restore.sh backups/2026-05-29T15-00-00Z
# =============================================================================

set -euo pipefail

SKIP_CONFIRM=0
if [[ "${1:-}" == "--yes" ]]; then
  SKIP_CONFIRM=1
  shift
fi

BACKUP="${1:-}"
if [[ -z "$BACKUP" ]]; then
  cat <<EOF >&2
usage: $0 [--yes] <backup_dir>

available backups:
EOF
  find ./backups -mindepth 1 -maxdepth 1 -type d 2>/dev/null | sort >&2 \
    || echo "  (none in ./backups)" >&2
  exit 2
fi

if [[ ! -d "$BACKUP" ]]; then
  echo "[restore] backup dir not found: $BACKUP" >&2
  exit 1
fi
if [[ ! -f "$BACKUP/postgres.dump.gz" ]]; then
  echo "[restore] missing $BACKUP/postgres.dump.gz" >&2
  exit 1
fi
if [[ ! -d "$BACKUP/storage" ]]; then
  echo "[restore] missing $BACKUP/storage/" >&2
  exit 1
fi

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env || true
  set +a
fi
: "${STORAGE_ROOT:?STORAGE_ROOT must be set (in .env or env)}"

# --- Confirmation ----------------------------------------------------------
if [[ "$SKIP_CONFIRM" -eq 0 ]]; then
  cat <<EOF

================================================================================
                              !! DESTRUCTIVE !!
================================================================================
About to restore from:
  $BACKUP

This will:
  1. Stop ALL running containers (api, cleaner, worker, dashboard, nginx)
  2. DROP and recreate the postgres database
  3. WIPE the contents of $STORAGE_ROOT
  4. Restore both from the backup
  5. Restart the services

If you have changes since this backup, they will be LOST.
================================================================================

EOF
  read -r -p "Type the word RESTORE (uppercase) to proceed: " ack
  if [[ "$ack" != "RESTORE" ]]; then
    echo "[restore] aborted"
    exit 1
  fi
fi

# --- 1. Stop everything except postgres ------------------------------------
echo "[restore] stopping app containers"
docker compose stop api cleaner worker dashboard nginx 2>/dev/null || true

# --- 2. Wipe + restore postgres --------------------------------------------
echo "[restore] recreating database"
docker compose exec -T postgres sh -c '
  psql -U "$POSTGRES_USER" -d postgres -c "DROP DATABASE IF EXISTS \"$POSTGRES_DB\";" \
                                       -c "CREATE DATABASE \"$POSTGRES_DB\" OWNER \"$POSTGRES_USER\";"
'

echo "[restore] loading dump"
gunzip -c "$BACKUP/postgres.dump.gz" \
  | docker compose exec -T postgres sh -c \
    'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --no-owner --no-acl' \
  || {
    echo "[restore] pg_restore reported errors — check with: docker compose logs postgres" >&2
    exit 1
  }

# --- 3. Wipe + restore storage ---------------------------------------------
echo "[restore] wiping $STORAGE_ROOT"
# Only wipe the dirs we own (buckets, segments, .cleanup); leave anything else.
for sub in buckets segments .cleanup; do
  sudo rm -rf "${STORAGE_ROOT:?}/$sub" 2>/dev/null || true
done

echo "[restore] rsyncing storage from backup"
mkdir -p "$STORAGE_ROOT"
rsync -a "$BACKUP/storage/" "$STORAGE_ROOT/"

# --- 4. Restart everything -------------------------------------------------
echo "[restore] starting all services"
docker compose up -d

cat <<EOF

[restore] done. Tail logs to watch the cleaner come back up:

  docker compose logs -f cleaner api dashboard

The cleaner's backfill will re-stamp object_shard_index for any restored
buckets; safe to leave running.
EOF
