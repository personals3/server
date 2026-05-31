# Production tuning

Checklist for taking a verified-working PersonalS3 deployment from "test
mode" to "I trust this with real data."

## 1. Backup, before anything else

Two scripts ship under `scripts/`:

| Script | What it does |
|---|---|
| `scripts/backup.sh` | One-shot backup: `pg_dump` → `postgres.dump.gz` + `rsync --link-dest` of `$STORAGE_ROOT`. Incremental — only changed files take new disk space. |
| `scripts/restore.sh` | Wipe + restore from a chosen backup directory. Interactive confirm. |

Verify they work BEFORE you flip live deletes on:

```bash
chmod +x scripts/backup.sh scripts/restore.sh
mkdir -p backups

# Make one backup
./scripts/backup.sh
ls -la backups/

# Restore-test in a throwaway shell (won't actually destroy if you say no
# at the prompt)
./scripts/restore.sh backups/$(ls -1 backups | tail -1)
# Type anything other than RESTORE → exits with "aborted"
```

Once verified, schedule daily backups:

```bash
crontab -e
# Add (adjust path):
0 3 * * * cd /home/you/Projects/S3orSimilar && ./scripts/backup.sh >> backups/backup.log 2>&1
```

Default rotation keeps the last 7 backups. Override via `KEEP_BACKUPS=30` in
`.env` (or in the cron line as `KEEP_BACKUPS=30 ./scripts/backup.sh`).

## 2. Dial cleaner knobs back from aggressive-test to production

After verifying the cleaner reacts to drops + cleans correctly (test
scenario in the V4 design doc), switch the knobs in `.env`:

```bash
# Production-safe (NOT what you used for testing)
CLEANUP_DRY_RUN=0                # actual deletes
ORPHAN_TWO_STRIKE=1              # require two consecutive sightings; 60s detection
ORPHAN_MIN_AGE_MINUTES=30        # protect mid-upload files

# Retention windows — adjust to your tolerance
TRASH_RETENTION_DAYS=30          # trash sits this long before purge
MULTIPART_EXPIRY_DAYS=7          # abandoned uploads cleaned after this
STUCK_TRANSCODE_MINUTES=120      # workers presumed dead after this
IMPORT_STUCK_HOURS=6

# Cadence
CLEANUP_INTERVAL=30s             # tick — V4 makes this cheap; OK to leave
WATCH_DISK=1                     # fsnotify watcher; turn off on networked FS
```

Apply:

```bash
docker compose up -d cleaner    # env-only change; no rebuild needed
```

Why dial back? With `ORPHAN_TWO_STRIKE=0 + ORPHAN_MIN_AGE_MINUTES=0`, a
file written at the wrong moment (race between an external tool and the
cleaner) could be reaped before its DB row lands. The 30-min grace +
two-strike rule essentially eliminates that risk.

## 3. Inotify limits (Linux only)

The fsnotify watcher uses one file descriptor per watched directory.
Default `fs.inotify.max_user_watches=8192` is fine for ~hundreds of
buckets; bump it for production:

```bash
sudo tee /etc/sysctl.d/99-personals3-inotify.conf > /dev/null <<EOF
fs.inotify.max_user_watches=524288
fs.inotify.max_user_instances=512
EOF
sudo sysctl -p /etc/sysctl.d/99-personals3-inotify.conf
```

Verify:

```bash
sysctl fs.inotify.max_user_watches
# → fs.inotify.max_user_watches = 524288
```

## 4. Postgres tuning

The bundled `postgres:16-alpine` ships with conservative defaults. For a
laptop-grade deployment with ~100K objects and a few users, tune in
`.env` (compose mounts these as `POSTGRES_INITDB_ARGS` aren't sufficient
for runtime; use `command:`):

For now the defaults handle 100K+ objects comfortably. When you scale past
1M objects per user, consider:

```yaml
# docker-compose.yml — postgres service
command: >
  postgres
  -c shared_buffers=256MB
  -c effective_cache_size=1GB
  -c work_mem=8MB
  -c maintenance_work_mem=128MB
  -c max_connections=100
  -c random_page_cost=1.1
```

The `random_page_cost=1.1` matters most — tells Postgres "you're on SSD,
prefer index scans." Without it, large bucket listings might fall back to
seq scans even with proper indexes.

## 5. Log rotation

Two log sources to manage:

**a) Docker container logs** — Docker rotates these automatically; cap
size in `docker-compose.yml` if you want tighter control:

```yaml
# each service:
logging:
  driver: json-file
  options:
    max-size: "20m"
    max-file: "5"
```

**b) Cleaner NDJSON audit log** — one file per day under
`$STORAGE_ROOT/.cleanup/`. Compress older files weekly:

```bash
crontab -e
# Add:
0 4 * * 0 find $STORAGE_ROOT/.cleanup -name '*.jsonl' -mtime +14 -exec gzip {} \;
0 5 * * 0 find $STORAGE_ROOT/.cleanup -name '*.jsonl.gz' -mtime +180 -delete
```

## 6. Weekly quota reconciliation

The cleaner enforces disk↔DB consistency but NOT quota↔reality. Use the
canonical reconcile script — it knows about every quota contributor:

```sql
-- scripts/quota-reconcile.sql (shipped in the repo)
used_bytes = SUM(size_bytes + transcoded_bytes + transcode_reserved_bytes)  -- objects
           + SUM(object_versions.size_bytes)                                  -- snapshots
```

Run it manually whenever you suspect drift (one-shot, idempotent):

```bash
docker compose exec -T postgres psql -U s3admin -d personals3 \
  -f - < scripts/quota-reconcile.sql
```

Or schedule via cron (recommended once a week):

```bash
0 6 * * 0 cd /home/you/Projects/S3orSimilar && \
  docker compose exec -T postgres sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  < scripts/quota-reconcile.sql >> backups/quota.log 2>&1
```

The script prints per-user drift first (read-only audit), then UPDATEs.
Healthy systems show `drift = 0 bytes` for every user. Common causes of
non-zero drift:

- Bucket force-delete bugs (historical; fixed in v4.x)
- Crashed worker mid-transcode (the FFmpeg-fail path now refunds — but
  if the container was killed instead of the worker process, the row
  stays charged until reconcile)
- Manual SQL changes

## 7. Monitoring checklist

A light-weight production deployment should at minimum:

- **Disk usage alert** — `df -h $STORAGE_ROOT` daily, email when > 90%
- **Cleaner heartbeat** — every cleanup_runs row → ensure they're appearing
  every <interval+10s>
- **API healthcheck** — already wired (`/healthz`); add an uptime ping
  to it from outside (e.g. cron + curl + email on failure)
- **Backup heartbeat** — `find backups -mindepth 1 -mtime -1` should
  show today's directory; alert if missing

Quick one-liner for "is the system healthy right now":

```bash
docker compose ps --format json \
  | jq -r '.[] | "\(.Service): \(.State)"' \
  | column -t
```

Should show all `running`, no `exited` or `restarting`.

## 8. Hardening (optional but recommended)

- **Off-site backups** — `rclone sync ./backups remote:personals3-backups`
  in the daily cron, with `--password-command` for encrypted credentials.
- **Backup encryption** — pipe the postgres dump through `gpg -c` before
  writing.
- **HTTPS termination** — currently nginx serves plain HTTP on port 80.
  If exposing publicly via Cloudflare tunnel, that's fine (tunnel is TLS).
  If exposing directly, add Caddy or certbot in front.
- **Rate limiting** — already done via Valkey for the API; review your
  middleware limits if you'll have public-facing endpoints.

## 9. What to monitor for slow drift

These aren't urgent failures but accumulate over months:

| Symptom | Cause | Fix |
|---|---|---|
| Cleaner ticks taking >100ms with no work | Bloom rebuild stale | Restart cleaner (rebuilds bloom from scratch) |
| Trash never shrinks | `TRASH_RETENTION_DAYS=0` accidentally OR cron clock skew | Verify cron + retention |
| `users.used_bytes` shows > actual | Bucket-delete race (fixed in v4.x); historical | Run weekly quota reconcile |
| Shard tree has thousands of leaves with `object_count=0` | Lots of object churn caused splits then deletes | Add a `prune_empty_leaves` task (future enhancement) |

For each: check `docker compose logs cleaner | grep -E 'error|fail'`
first. Most issues surface there clearly.
