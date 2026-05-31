#!/usr/bin/env bash
# =============================================================================
# PersonalS3 backup — postgres dump + storage rsync.
#
# Each run writes a NEW directory under $BACKUP_ROOT named with an ISO
# timestamp. Subsequent runs use rsync --link-dest against the previous
# backup, so unchanged files are hardlinked (zero extra disk per backup).
# Net cost per nightly backup ≈ size of the day's changes + ~10 MB metadata.
#
# Restore with: scripts/restore.sh <backup_dir>
#
# Knobs (via env or .env):
#   BACKUP_ROOT    where backups live (default ./backups)
#   STORAGE_ROOT   the live storage tree to back up
#   KEEP_BACKUPS   how many recent backups to keep (default 7)
#   BACKUP_GZIP    gzip level for pg dump (1..9; default 1 — fast, good ratio)
#
# Exit codes: 0 ok, non-zero on any failure (this script is `set -e`).
# =============================================================================

set -euo pipefail

if [[ -f .env ]]; then
  set -a
  # shellcheck disable=SC1091
  source .env || true
  set +a
fi

: "${BACKUP_ROOT:=./backups}"
: "${STORAGE_ROOT:?STORAGE_ROOT must be set (in .env or env)}"
: "${KEEP_BACKUPS:=7}"
: "${BACKUP_GZIP:=1}"

STAMP="$(date -u +%Y-%m-%dT%H-%M-%SZ)"
DEST="$BACKUP_ROOT/$STAMP"

# Previous backup (for --link-dest). Empty if this is the first.
PREV=""
if [[ -d "$BACKUP_ROOT" ]]; then
  PREV=$(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d 2>/dev/null \
         | sort | tail -1)
fi

mkdir -p "$DEST"

# --- 1. Postgres dump --------------------------------------------------------
echo "[backup] dumping postgres -> $DEST/postgres.dump.gz"
docker compose exec -T postgres sh -c \
  'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc --no-owner --no-acl' \
  | gzip -"$BACKUP_GZIP" > "$DEST/postgres.dump.gz"

PG_BYTES=$(stat -c '%s' "$DEST/postgres.dump.gz" 2>/dev/null \
           || stat -f '%z' "$DEST/postgres.dump.gz")
echo "[backup] postgres dump: $((PG_BYTES / 1024 / 1024)) MB"

# --- 2. Storage rsync (incremental via --link-dest) --------------------------
echo "[backup] rsync storage -> $DEST/storage/"
if [[ -n "$PREV" && -d "$PREV/storage" ]]; then
  rsync -a --link-dest="$(realpath "$PREV/storage")" \
        "$STORAGE_ROOT"/ "$DEST/storage"/
  echo "[backup] incremental against $PREV"
else
  rsync -a "$STORAGE_ROOT"/ "$DEST/storage"/
  echo "[backup] full (no previous backup)"
fi

ST_BYTES=$(du -sb "$DEST/storage" 2>/dev/null | cut -f1 \
           || du -sk "$DEST/storage" | awk '{print $1 * 1024}')
ST_REAL=$(du -sb --apparent-size "$DEST/storage" 2>/dev/null | cut -f1 \
           || echo "$ST_BYTES")
echo "[backup] storage size: $((ST_BYTES / 1024 / 1024)) MB physical, " \
     "$((ST_REAL / 1024 / 1024)) MB logical"

# --- 3. Metadata sidecar -----------------------------------------------------
cat > "$DEST/metadata.json" <<EOF
{
  "stamp": "$STAMP",
  "previous": "$(basename "$PREV" 2>/dev/null || echo '')",
  "storage_path": "$STORAGE_ROOT",
  "postgres_dump_bytes": $PG_BYTES,
  "storage_physical_bytes": $ST_BYTES,
  "tooling": {
    "rsync": "$(rsync --version | head -1 | awk '{print $3}')",
    "gzip":  "$(gzip --version | head -1 | awk '{print $2}')"
  }
}
EOF

# --- 4. Rotation: keep only the last N -------------------------------------
if [[ "$KEEP_BACKUPS" -gt 0 ]]; then
  KEEP=$(find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d \
         | sort | tail -n "$KEEP_BACKUPS")
  find "$BACKUP_ROOT" -mindepth 1 -maxdepth 1 -type d \
    | sort \
    | grep -v -F "$KEEP" 2>/dev/null \
    | while read -r old; do
        echo "[backup] pruning old: $(basename "$old")"
        rm -rf "$old"
      done || true
fi

echo "[backup] done: $DEST"
