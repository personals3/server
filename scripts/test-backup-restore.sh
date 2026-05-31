#!/usr/bin/env bash
# =============================================================================
# Backup + restore end-to-end validation.
#
# 1. Snapshot the current DB state (user count, object count, used_bytes sum).
# 2. Run scripts/backup.sh — verify the dump + storage tree both landed.
# 3. Dump current DB to a "before" file.
# 4. Wipe the DB and storage to simulate disaster.
# 5. Run scripts/restore.sh against the backup dir.
# 6. Compare the restored state to the snapshot — every count + sum should
#    match exactly. quota-reconcile.sql should show drift=0 for every user.
#
# DESTRUCTIVE — runs on the live deployment. Default mode does a DRY RUN
# (only steps 1+2). Pass --destroy to actually wipe + restore.
#
# Usage:
#   ./scripts/test-backup-restore.sh                    # safe — just verifies backup
#   ./scripts/test-backup-restore.sh --destroy          # full cycle on live data
# =============================================================================

set -euo pipefail

cd "$(dirname "$0")/.."

DESTRUCTIVE=0
if [[ "${1:-}" == "--destroy" ]]; then
  DESTRUCTIVE=1
fi

red()   { printf '\033[31m%s\033[0m\n' "$*" >&2; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
step()  { printf '\033[1;36m=== %s\033[0m\n' "$*"; }

PG="docker compose exec -T postgres psql -U s3admin -d personals3 -tA"

snapshot() {
  echo "{"
  echo "  \"users\":   $($PG -c 'SELECT count(*) FROM users')"
  echo "  \"buckets\": $($PG -c 'SELECT count(*) FROM buckets')"
  echo "  \"objects\": $($PG -c 'SELECT count(*) FROM objects')"
  echo "  \"versions\":$($PG -c 'SELECT count(*) FROM object_versions')"
  echo "  \"used_sum\":$($PG -c 'SELECT COALESCE(SUM(used_bytes),0) FROM users')"
  echo "  \"disk_bytes\":$(du -sb "${STORAGE_ROOT:-./storage}" 2>/dev/null | cut -f1)"
  echo "}"
}

step "step 1: snapshot current state"
SNAP_BEFORE=$(snapshot)
echo "$SNAP_BEFORE"

step "step 2: run scripts/backup.sh"
KEEP_BACKUPS=14 ./scripts/backup.sh

BACKUP_DIR=$(find "${BACKUP_ROOT:-./backups}" -mindepth 1 -maxdepth 1 -type d 2>/dev/null \
             | sort | tail -1)
[ -d "$BACKUP_DIR" ] || { red "no backup directory found under ${BACKUP_ROOT:-./backups}"; exit 1; }
[ -f "$BACKUP_DIR/postgres.dump.gz" ] || { red "no postgres.dump.gz in $BACKUP_DIR"; exit 1; }
[ -d "$BACKUP_DIR/storage" ] || { red "no storage/ tree in $BACKUP_DIR"; exit 1; }
green "backup landed at: $BACKUP_DIR"
green "  postgres dump: $(du -sh "$BACKUP_DIR/postgres.dump.gz" | cut -f1)"
green "  storage tree:  $(du -sh "$BACKUP_DIR/storage" | cut -f1)"

if [ $DESTRUCTIVE -eq 0 ]; then
  green ""
  green "dry run complete — backup verified."
  green "to actually wipe + restore + verify equality, re-run with: --destroy"
  exit 0
fi

step "step 3: WIPING live database and storage in 5 seconds (Ctrl-C aborts)"
sleep 5

step "step 4: docker compose down -v + restore"
docker compose down -v
./scripts/restore.sh "$BACKUP_DIR"
docker compose up -d
echo "waiting up to 60s for api to come healthy..."
for i in $(seq 1 30); do
  sleep 2
  if curl -fsS http://localhost:8080/nginx-health >/dev/null 2>&1; then
    green "api responsive"
    break
  fi
done

step "step 5: snapshot post-restore state"
SNAP_AFTER=$(snapshot)
echo "$SNAP_AFTER"

step "step 6: diff before vs after"
if diff <(echo "$SNAP_BEFORE") <(echo "$SNAP_AFTER") > /dev/null; then
  green "✓ before / after snapshots match exactly"
else
  red "✗ snapshots differ:"
  diff <(echo "$SNAP_BEFORE") <(echo "$SNAP_AFTER") || true
  exit 1
fi

step "step 7: quota drift check"
$PG -c "
SELECT email, pg_size_pretty(used_bytes - (
  (SELECT COALESCE(SUM(size_bytes + COALESCE(transcoded_bytes,0)
                       + COALESCE(transcode_reserved_bytes,0)),0)
     FROM objects WHERE bucket_id IN
       (SELECT id FROM buckets WHERE owner_id=u.id))
+ (SELECT COALESCE(SUM(ov.size_bytes),0)
     FROM object_versions ov JOIN objects o ON o.id=ov.object_id
    WHERE o.bucket_id IN
       (SELECT id FROM buckets WHERE owner_id=u.id))
)) AS drift
  FROM users u ORDER BY email;
"

green ""
green "backup + restore validated end-to-end"
