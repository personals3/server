# Operations

Day-2: monitoring, backup, scaling, logs.

---

## Logs

```bash
# All services
docker compose logs -f

# One service
docker compose logs -f api
docker compose logs -f worker
docker compose logs -f nginx
docker compose logs -f postgres

# Last N lines
docker compose logs --tail=50 api
```

| Log line you'll see often | Means |
|---|---|
| `connected to postgres` / `connected to valkey` | api startup |
| `listening on 0.0.0.0:3000` | api ready |
| `job <uuid> done` | worker finished a transcode |
| `[req-id] "GET /api/..." from <ip> - 200 ... in 1.23ms` | per-request chi log |
| `SIGV4 MISMATCH ...` | only if you set `DEBUG_SIGV4=1` |
| `audit log write failed: ...` | postgres unreachable; non-fatal |

---

## Backup the database

The metadata (users, quotas, audit log, objects) is invaluable. Back it up:

```bash
# Snapshot to a tarball (atomic via pg_dump)
docker compose exec -T postgres pg_dump -U s3admin -d personals3 \
  > backups/personals3-$(date +%F).sql

# Restore
docker compose exec -T postgres psql -U s3admin -d personals3 \
  < backups/personals3-2026-05-29.sql
```

Automated nightly cron:
```cron
0 4 * * * cd /home/me/Projects/S3orSimilar && docker compose exec -T postgres pg_dump -U s3admin -d personals3 | gzip > /mnt/backups/personals3-$(date +\%F).sql.gz
```

---

## Backup the object data

The bytes themselves live in `STORAGE_ROOT`. Treat it like any other
filesystem backup:

```bash
# rsync to a backup disk
rsync -av --delete ~/Projects/S3orSimilar/storage/ /mnt/backupdisk/personals3/

# Or use PersonalS3 to back itself up to PersonalS3 (recursive!)
# — better: use a different remote (rclone to AWS S3 / Backblaze B2)
rclone sync ~/Projects/S3orSimilar/storage b2:personals3-backup/
```

To restore: stop the stack, copy back, start. The DB references are
path-based, so as long as `STORAGE_ROOT` is the same, things just work.

---

## Scaling

### Scale transcode workers (CPU-bound)

```bash
# How many CPU cores do you have?
nproc

# Match it (one ffmpeg process per worker)
docker compose up -d --scale worker=8
```

Each worker uses `FOR UPDATE SKIP LOCKED` so they never grab the same job —
linear speedup until you saturate CPU or disk bandwidth.

### Scale the API (CPU-bound for many small requests)

The Go API is heavily concurrent inside one process. Usually one container
is plenty for a personal server. If you really need more:

```yaml
# docker-compose.yml — would need a second nginx upstream entry too
api:
  scale: 3   # ← not currently in our compose but possible
```

Nginx already keeps a connection pool to api:3000 — adding more API
containers needs a small `upstream { server api-1; server api-2; ... }`
adjustment.

### Scale the database

We use one PostgreSQL container. If you ever need more:
- Read replicas: doable, but the API doesn't currently route reads
  separately
- Bigger: increase shared_buffers in postgres command, give the container
  more memory

For typical personal use (10-100 users, millions of objects, billions of
rows) the default is fine.

### Storage I/O

Bottleneck is your disk's sequential read/write bandwidth.
- Single HDD: ~150 MB/s — fine for streaming a couple of HLS viewers
- SSD: ~500 MB/s — comfortable for many concurrent viewers
- NVMe: 2+ GB/s — overkill for personal use

Cloudflare caches HLS `.ts` segments at the edge, so popular content only
hits your disk once.

---

## Monitor health

### Built-in endpoints

```bash
# Each layer has its own
curl http://localhost:8080/nginx-health     # nginx alone
curl http://localhost:8080/api/healthz      # nginx + api + postgres
```

### Stats endpoint

```bash
JWT=$(...)  # admin login

curl -s -H "Authorization: Bearer $JWT" \
  http://localhost:8080/api/admin/stats | jq
```

Key fields to alert on:
- `disk.physicalUsed / disk.physicalTotal` → physical disk %
- `disk.overcommitted` → boolean, true = you've promised more than you have
- `transcodeJobs.failed` → growing = check worker logs
- `requestsLastHour` → traffic baseline

Per-user storage drift (run weekly via cron, see
[production-tuning.md](./production-tuning.md#6-weekly-quota-reconciliation)):

```bash
docker compose exec -T postgres psql -U s3admin -d personals3 \
  -f - < scripts/quota-reconcile.sql
```

Should show `drift = 0 bytes` for every user. Non-zero means a crashed
mid-write or manual SQL edit; the script prints the audit then UPDATEs.

Per-user breakdown (same data the dashboard's storage chart uses):

```bash
curl -s -H "Authorization: Bearer $JWT" \
  http://localhost:8080/api/auth/storage | jq
# {
#   "quotaBytes": ..., "usedBytes": ...,
#   "buckets": [{name, originalBytes, transcodedBytes, reservedBytes, trashBytes, ...}],
#   "trashBytes": ..., "versionsBytes": ..., "reservedBytes": ...
# }
```

### Audit log queries

```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  -- Top 10 hottest users in the last day
  SELECT u.email, COUNT(*) AS reqs
    FROM audit_log a JOIN users u ON u.id = a.user_id
   WHERE a.ts > now() - INTERVAL '1 day'
   GROUP BY u.email ORDER BY reqs DESC LIMIT 10;"

docker compose exec postgres psql -U s3admin -d personals3 -c "
  -- Recent failures
  SELECT ts, action, bucket_name, object_key, status_code
    FROM audit_log
   WHERE status_code >= 400
   ORDER BY ts DESC LIMIT 30;"
```

---

## Reset things

```bash
# Restart one service (after editing code)
docker compose up -d --build api

# Restart with config reload (e.g. after nginx.conf changes)
docker compose restart nginx

# Clear rate limits (Valkey)
docker compose exec valkey valkey-cli -a "$(grep VALKEY_PASSWORD .env|cut -d= -f2)" FLUSHDB

# Nuke EVERYTHING (db, valkey state, storage)
docker compose down -v
rm -rf ~/Projects/S3orSimilar/storage/{buckets,segments}
docker compose up -d
```

---

## Upgrading

```bash
cd ~/Projects/S3orSimilar
git pull           # if you've cloned, otherwise update files manually
docker compose down
docker compose up -d --build
```

If a new migration was added (e.g., `db/migrations/006_xxx.sql`), it only
runs against a *fresh* database. To apply against an existing DB:

```bash
docker compose exec -T postgres psql -U s3admin -d personals3 \
  < db/migrations/006_xxx.sql
```

---

## Sizing guidance

For a personal/family server (10 people, photo/video sharing):

| Resource | Recommended |
|---|---|
| RAM | 4 GB (postgres + 1 api + 2 workers comfortably) |
| CPU | 4 cores (2 workers transcoding, rest for API + nginx) |
| Disk | 1 TB SSD, ext4/xfs/btrfs/zfs all fine |
| Network | Symmetric 100 Mbps for video streaming to remote viewers |

For something larger, scale workers first, then disk, then API replicas.
