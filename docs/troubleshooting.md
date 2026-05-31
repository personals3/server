# Troubleshooting

When things break, work through this list.

## Quick triage

```bash
docker compose ps        # What's running, what's healthy
docker compose logs --tail=30 <service>
```

Most issues are visible in the first 30 log lines of the offending service.

---

## Container won't start

### `s3-api` keeps restarting, logs show `mkdir /storage/buckets: permission denied`

Your `UID`/`GID` in `.env` doesn't match the owner of `STORAGE_ROOT`.

```bash
# Check
id -u && id -g
ls -ld $STORAGE_ROOT

# Fix
chown -R $(id -u):$(id -g) $STORAGE_ROOT
# Or set UID= and GID= in .env to match
```

### `bind: address already in use` on port 8080 / 3000 / 5432 / 6379

Something else on your host is using that port.

```bash
# Find what
sudo ss -tlnp | grep ':8080'

# Either stop that service, or change the port:
# In .env, change NGINX_HTTP_PORT=8080 to 8081, etc.
```

### Postgres won't come healthy

```bash
docker compose logs postgres | tail -30
```

Common causes:
- **Volume permission issue:** `docker volume rm s3-postgres-data && docker compose up -d`
- **Disk full:** `df -h`
- **Old data with newer image:** migration mismatch → wipe and restart `docker compose down -v`

---

## "I can't log in"

### Dashboard says `invalid credentials`

You changed the admin password and forgot it, or the seed didn't run.

**Reset:**
```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  UPDATE users
     SET password_hash = crypt('admin', gen_salt('bf', 10))
   WHERE email = 'admin@local';"
```

Now log in as `admin@local` / `admin`.

### Dashboard loads but every API call returns 401

Stale JWT in `localStorage`. Open browser console:
```javascript
localStorage.removeItem('ps3_token')
// then reload
```

### `Unauthorized` from a script with a fresh API key

Check what you saved:
```bash
echo "$PS3_KEY" | head -c 30
# Should start with "psk_" then 8 hex chars then "."
# If yours doesn't, you copied the wrong field
```

The key value is in `key` field of the response, not `id` (that's a UUID).

---

## Uploads fail

### `507 Insufficient Storage` / `code: QUOTA_EXCEEDED`

Your user quota is full. The response body now includes a `details`
block telling you exactly what's needed:

```json
{ "code": "QUOTA_EXCEEDED",
  "details": { "requestedBytes": 5242880,
               "availableBytes": 0,
               "deficitBytes":   5242880,
               "usedBytes":      2147483648,
               "quotaBytes":     2147483648 } }
```

Either:
- Delete files (or empty Trash if you previously soft-deleted)
- Free transcoded segments — `DELETE /:bucket/:key?transcodes` keeps the
  original, refunds HLS bytes
- Ask an admin to increase your quota (Dashboard → Admin → Users → edit)
- Watch for **stuck reservations** if pre-flight ran but no job picked it up:
  ```bash
  docker compose exec -T postgres psql -U s3admin -d personals3 -c "
    SELECT key, transcode_status, pg_size_pretty(transcode_reserved_bytes)
      FROM objects WHERE transcode_reserved_bytes > 0;"
  ```
  If any row has been `pending` for hours with no worker activity, the
  worker likely crashed mid-claim. Reset it:
  ```sql
  UPDATE objects SET transcode_status='none', transcode_reserved_bytes=0
   WHERE id='...';
  DELETE FROM transcode_jobs WHERE object_id='...';
  -- then refund manually
  UPDATE users SET used_bytes = used_bytes - <reserved>
   WHERE id = (SELECT owner_id FROM buckets WHERE id = (
                 SELECT bucket_id FROM objects WHERE id='...'));
  ```
  Or just run `scripts/quota-reconcile.sql` which corrects drift in one shot.

### `507 Insufficient Storage` / `code: DISK_FULL`

The physical disk is past `disk_full_threshold_pct` (default 95%).
Admin needs to:
- Free up disk space (`du -sh ~/Projects/S3orSimilar/storage/*/*`)
- Or raise threshold (`Admin → System → Storage Configuration`)
- Or add more disk (see [storage-management.md](./storage-management.md))

### `Connection reset` mid-upload (large file)

- Could be a network hiccup — large multipart uploads survive transient
  errors as long as the chunks complete
- Could be a CDN or Cloudflare timeout (their default is 100 seconds per
  request) — use multipart for files > 100 MB

### "It uploaded but I can't see it in the dashboard"

Refresh the page. If still missing:
```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  SELECT bucket_id, key, size_bytes, created_at FROM objects
   ORDER BY created_at DESC LIMIT 5;"
```

If the row's there but the dashboard didn't list it — check browser dev
tools for the API response from `GET /:bucket`.

---

## Streaming / playback issues

### Video player shows error, but `/stream/{id}/master.m3u8` returns 200

Check the `master.m3u8` content — is there a `404` segment file referenced?
Transcode may have failed partway:

```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  SELECT status, error FROM transcode_jobs
   WHERE object_id = '...' ORDER BY created_at DESC LIMIT 1;"
```

### Browser says `CORS error` when loading the player

Check the response headers from a `.m3u8`:
```bash
curl -I http://localhost:8080/stream/{id}/master.m3u8
```

You should see `Access-Control-Allow-Origin: *`. If not, nginx didn't
reload the config. `docker compose restart nginx`.

### HLS plays but always at the lowest quality

Browser bandwidth detection is conservative. Force a quality in video.js
or wait for it to ramp up.

---

## Performance

### Dashboard feels slow

```bash
# Are you running on a Mac with Docker Desktop?
# File system bind-mounts are slow on Mac/Windows. Use named volumes for STORAGE_ROOT:
# (in docker-compose.yml change)
#   - ${STORAGE_ROOT}:/storage
# to:
#   - storage_data:/storage
# And add to volumes section:
#   storage_data:
```

### API logs show `(audit log write failed)`

PostgreSQL is overloaded. Check connections:
```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  SELECT count(*), state FROM pg_stat_activity GROUP BY state;"
```

We open up to 20 pgxpool connections per API. If exhausted, audit writes
queue up. Restart api or scale postgres.

### Transcode jobs stuck in `pending`

```bash
docker compose ps | grep worker
docker compose logs worker | tail -50
```

If no workers are running:
```bash
docker compose up -d worker
```

If they're running but not picking up jobs, check for stuck locks:
```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  SELECT * FROM transcode_jobs WHERE status = 'processing' AND started_at < now() - INTERVAL '10 minutes';"

-- Reset stuck ones to pending:
docker compose exec postgres psql -U s3admin -d personals3 -c "
  UPDATE transcode_jobs SET status = 'pending', worker_id = NULL
   WHERE status = 'processing' AND started_at < now() - INTERVAL '10 minutes';"
```

### Object stuck in `transcode: skipped_quota` or `failed_quota`

These are end states; nothing is wrong with the file (original plays
fine). The transcode pre-flight (or publish-point) refused because your
quota wouldn't fit the HLS ladder.

Inspect the diagnostic:

```bash
docker compose exec -T postgres psql -U s3admin -d personals3 -c "
  SELECT key, transcode_status, transcoded->'quota'
    FROM objects WHERE transcode_status IN ('skipped_quota','failed_quota');"
```

You'll see `deficitBytes` — that's how much you need to free, then click
**Retry (need more space)** in the dashboard (or `DELETE /:b/:k?transcodes`
via API).

### Worker logs spam `cancel subscriber error (Timeout reading from socket)`

Fixed in current builds — `redis.from_url(socket_timeout=None,
health_check_interval=30)` + `pubsub.get_message(timeout=5.0)` keeps the
subscriber connected across NAT idle-timeouts without falling into the
reconnect loop.

If you still see it: pull latest, rebuild worker:
```bash
docker compose up -d --build worker
```

The subscriber receives `transcode:cancel` Valkey messages so the worker
can SIGTERM in-flight FFmpeg the moment you delete or replace an object.
Job processing falls back to DB-poll when the subscriber is down, so this
was always a UX bug, never a correctness one.

### `Cancel & restart` says "Queued — waiting for a worker..." but nothing happens

The dashboard UI was previously falling through to that copy for the new
`skipped_quota` / `failed_quota` states. Fixed in current builds — the
panel now shows a labeled deficit table and the button reads "Retry (need
more space)". If you still see the old copy, rebuild the dashboard:
```bash
docker compose up -d --build dashboard
```

---

## SigV4 / AWS CLI issues

### `aws s3 ls` returns `expected string or bytes-like object, got NoneType`

The server is returning an error response that aws-cli can't parse.
Common cause: endpoint URL must include `/api`:

```bash
# Wrong
aws --endpoint-url=https://s3.yourdomain.com s3 ls

# Right
aws --endpoint-url=https://s3.yourdomain.com/api s3 ls
```

### `SignatureDoesNotMatch`

Common causes:
- Clock skew between you and the server > 15 minutes (we reject old signatures)
- nginx not passing the right `Host` header (we use `$http_host` to preserve port — check `nginx.conf`)
- Path stripping mismatch — see API logs with `DEBUG_SIGV4=1`:
  ```bash
  # Add DEBUG_SIGV4: "1" to api service environment in docker-compose.yml
  docker compose up -d api
  docker compose logs api | grep SIGV4
  ```

### `InvalidAccessKeyId`

The credential was revoked, expired, or never existed. Check:
```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  SELECT access_key_id, user_id, name, last_used_at
    FROM s3_credentials;"
```

---

## Cloudflare tunnel issues

### `502 Bad Gateway`

cloudflared can't reach your service. Test from the cloudflared host:
```bash
curl http://localhost:8080/nginx-health
```

If that works but tunnel fails, your tunnel's "Service URL" is wrong.
See [cloudflare-tunnel.md](./cloudflare-tunnel.md).

### `Cloudflare Error 1033: Argo Tunnel error`

cloudflared isn't running. On the host:
```bash
sudo systemctl restart cloudflared
# or
docker compose restart cloudflared
```

### Tunnel works but dashboard shows CORS errors

`NEXT_PUBLIC_API_URL` mismatch. The dashboard calls whatever you compiled into
it. For same-origin tunnel use:
```bash
# In .env
NEXT_PUBLIC_API_URL=/api
docker compose up -d --build dashboard
```

---

## Database integrity checks

### Quota drift

Use the canonical reconcile script — it knows about `transcoded_bytes`,
`transcode_reserved_bytes`, soft-deleted rows, and `object_versions`:

```bash
docker compose exec -T postgres psql -U s3admin -d personals3 \
  -f - < scripts/quota-reconcile.sql
```

It prints per-user drift first, then UPDATEs. Healthy systems show
`drift = 0 bytes` for every user. Run it weekly via cron (see
`production-tuning.md`) and any time you suspect `used_bytes` is wrong.

### Orphan segments on disk

The cleaner reaps these automatically every 30 s (see
`storage-management.md#reclaim-space`). To spot-check:

```bash
docker compose exec api sh -c '
  for d in /storage/segments/*/; do
    oid=$(basename "$d")
    psql "$DATABASE_URL" -tA -c "SELECT 1 FROM objects WHERE id='\''$oid'\''
      AND transcode_status IN ('\''done'\'','\''pending'\'','\''processing'\'')" \
      | grep -q 1 || echo "ORPHAN: $d"
  done'
```

Anything printed will be reaped within ~60 s. If the same path persists
across multiple ticks, check `cleanup_runs.errors` and ensure
`CLEANUP_DRY_RUN=0`.

---

## Still stuck?

```bash
# Capture a debug bundle to share
docker compose ps > /tmp/debug.txt
docker compose logs --tail=200 >> /tmp/debug.txt
docker compose exec postgres psql -U s3admin -d personals3 -c "
  SELECT current_setting('server_version'), pg_database_size('personals3');" >> /tmp/debug.txt
df -h ~/Projects/S3orSimilar/storage >> /tmp/debug.txt

cat /tmp/debug.txt
```

That gives you (or anyone helping) enough to diagnose 95% of issues.
