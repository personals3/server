# Low-Level Component Reference

Each container, its responsibilities, its internals, its config.

---

## 1. PostgreSQL (`s3-postgres`)

**Image:** `postgres:16-alpine`
**Port:** 5432 (exposed to host)
**Volume:** `s3-postgres-data` (Docker named volume — survives `down`)

### What it stores

```
users               accounts, password hashes, quotas, used_bytes
api_keys            bcrypt-style Bearer tokens (psk_...)
s3_credentials      AWS-style AKID + secret_access_key
buckets             namespaces per user
objects             every uploaded file, metadata, transcoded info
multipart_uploads   in-progress uploads (state)
multipart_parts     per-part rows (one per chunk)
transcode_jobs      queue table polled by workers
audit_log           append-only event log
system_config       admin-tunable runtime knobs
```

See [database-schema.md](./database-schema.md) for the full SQL.

### Why PostgreSQL and not SQLite

- **Atomic quota accounting** requires `UPDATE ... WHERE used + delta <= quota`
  with row-level locking. SQLite would serialize all writes.
- **Concurrent multipart uploads** need `FOR UPDATE SKIP LOCKED` (Postgres 9.5+).
- **JSONB** for flexible object metadata (transcoded paths).
- **CHECK constraints** enforce invariants (`used_bytes >= 0`).

### Migrations

Auto-applied from `db/migrations/*.sql` on first startup only.
Files run in alphabetical order. To add a migration, drop a new
`006_xxx.sql` and run `docker compose down -v && docker compose up -d`
(yes, you lose data — this is a dev environment, not production).

---

## 2. Valkey (`s3-valkey`)

**Image:** `valkey/valkey:7-alpine`
**Port:** 6379 (exposed)
**Volume:** `s3-valkey-data` (RDB snapshots every 60s if ≥100 keys changed)

Valkey is a drop-in Redis replacement (BSD-licensed). Same protocol, same
commands, same client libraries.

### What it stores (all ephemeral, all keyed)

```
ratelimit:{user-id}:{unix-minute}      INT counter, TTL 65s
                                       INCR'd on every request
```

Currently that's it. Future uses:
- Multipart upload progress (real-time UI)
- Refresh-token blocklist for revoked JWTs

### Why fail-open on Valkey errors

If Valkey is down, the rate limiter passes the request through rather than
denying everything. The API still works fully; you just lose rate limiting
until Valkey comes back.

---

## 3. Go API (`s3-api`)

**Image:** built locally from `api/Dockerfile` (multi-stage,  ~15 MB final).
**Port:** 3000 (internal; only nginx talks to it).
**Process:** statically compiled, runs as the host UID for storage permissions.

### Code layout

```
api/
├── cmd/server/main.go              entrypoint: wire DB+cache+FS, register routes
└── internal/
    ├── config/        env-var loader
    ├── db/            pgx connection pool (max 20 conns)
    ├── cache/         go-redis client for Valkey
    ├── storage/
    │   ├── fs.go          object layout + atomic writes + ETag (MD5)
    │   └── diskstats.go   syscall.Statfs wrapper
    ├── sysconfig/     get/set helpers for system_config table
    ├── auth/
    │   ├── password.go    bcrypt cost-10
    │   ├── apikey.go      psk_xxxx.yyy format, SHA-256 hash
    │   ├── jwt.go         HS256 24h tokens
    │   └── sigv4.go       AWS Signature V4 verifier
    ├── middleware/
    │   ├── auth.go        Bearer OR SigV4, attaches *User to context
    │   ├── ratelimit.go   Valkey sliding window
    │   ├── audit.go       async writes to audit_log
    │   └── quota.go       atomic UPDATE...WHERE used+delta<=quota
    ├── httpx/         JSON/error response helpers
    └── handlers/
        ├── auth.go        login, /auth/me, API key CRUD
        ├── s3creds.go     AKID/secret CRUD
        ├── admin.go       users CRUD, audit viewer, stats, system_config
        ├── buckets.go     create/list/delete/head buckets
        ├── objects.go     PUT/GET/HEAD/DELETE objects, ListObjectsV2
        ├── multipart.go   initiate/upload-part/complete/abort/list-parts
        ├── transcode.go   enqueueTranscode() called after successful PUT
        ├── s3xml.go       S3-compatible XML response types
        └── errors.go      pgx error classification (unique violation, etc.)
```

### Request lifecycle

```
HTTP Request
    │
    ▼
chi.Router → matches route pattern (e.g. /{bucket}/*)
    │
    ▼
Middleware stack (order matters):
  1. RequestID         attaches a UUID for log correlation
  2. RealIP            sets r.RemoteAddr from X-Forwarded-For
  3. Logger            logs method+path+status+latency
  4. Recoverer         catches panics, returns 500
  5. Timeout(60s)      cancels handler if it takes too long
    │
    ▼
On authenticated routes (everything except /healthz, /auth/login):
  6. Authenticator     parses Bearer/SigV4, loads user → ctx
  7. RateLimit         Redis INCR; reject if >1000/min (10000 for admin)
  8. Audit             wraps ResponseWriter to capture status, fires
                       async INSERT to audit_log after handler returns
    │
    ▼
Handler — has middleware.MustUser(ctx) to get *User
```

### Streaming uploads

`PutObject` reads the request body directly into `io.Copy(file, req.Body)`,
never buffering. Memory usage during a 5 GB upload: ~10 MB (the OS read
buffer). Concurrent uploads scale linearly.

### Atomic quota enforcement

```go
// 1. Pre-check: try to reserve N bytes
err := QuotaReserve(ctx, db, userID, contentLength)
// SQL: UPDATE users SET used_bytes = used_bytes + N
//        WHERE id = ? AND used_bytes + N <= quota_bytes
//      RETURNING TRUE
// 0 rows = quota exceeded → 507

// 2. Write to disk (might be smaller or bigger than Content-Length)
size, etag, err := fs.WriteObject(...)

// 3. Reconcile: adjust by the difference
QuotaReserve(ctx, db, userID, actualSize - reserved)

// 4. If the upload bombed, we refund the full reservation
```

---

## 4. Python Worker (`s3-worker-N`)

**Image:** built from `worker/Dockerfile` (Debian slim + ffmpeg + libaom + Pillow).
**Process:** infinite poll loop, no HTTP listener.
**Scale:** `docker compose up -d --scale worker=4` for parallel processing.

### Code layout

```
worker/worker/
├── main.py         poll loop, SIGTERM handler, idle heartbeat
├── db.py           psycopg3 + claim_job/complete_job/fail_job + publish-point
│                   quota settle (delta = actual - reserved - old_transcoded_bytes)
├── transcoder.py   dispatch by file_type
├── video.py        HLS multi-bitrate ladder + thumbnails + GPU/VAAPI when available
├── audio.py        HLS audio + MP3 + OGG
├── image.py        Pillow → WebP + thumbs; ffmpeg → AVIF
└── cancel.py       Valkey subscriber on transcode:cancel → SIGTERM running ffmpeg
                    (socket_timeout=None + get_message(timeout=5) for clean shutdown)
```

### Quota settlement at publish

`db.complete_job` runs at `video_finalize` time. The API already charged
`transcode_reserved_bytes` against `users.used_bytes` at enqueue. The
worker reconciles:

```python
delta_user = actual_segments_bytes - reserved - old_transcoded_bytes
if delta_user > 0:
    UPDATE users SET used_bytes = used_bytes + delta_user
     WHERE id = $1 AND used_bytes + delta_user <= quota_bytes
     RETURNING used_bytes
    if not charged:
        shutil.rmtree(segments_dir)
        UPDATE users SET used_bytes = used_bytes - reserved        # refund FULL
        UPDATE objects SET transcode_status='failed_quota',
                           transcode_reserved_bytes=0
elif delta_user < 0:
    UPDATE users SET used_bytes = GREATEST(used_bytes + delta_user, 0)
# happy path: clear reserved column, set transcoded_bytes = actual
```

`fail_job` (FFmpeg crashed past max_attempts) also refunds the
reservation and reaps the partial segments dir.

### Claiming jobs (the magic SQL)

```sql
UPDATE transcode_jobs SET
    status     = 'processing',
    started_at = now(),
    worker_id  = $1,
    attempts   = attempts + 1
WHERE id = (
    SELECT id FROM transcode_jobs
    WHERE status = 'pending' AND attempts < max_attempts
    ORDER BY priority, created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED   -- ← this is the magic
)
RETURNING id, object_id, input_path, output_dir, file_type, attempts, max_attempts;
```

`FOR UPDATE SKIP LOCKED` lets N workers poll concurrently without ever
grabbing the same row. PostgreSQL serializes the row lock atomically.

### Video transcoding ladder

Source resolution probed via ffprobe; ladder built dynamically. A 4K
source produces 5+ rungs; a 720p source only 3:

```go
// api/internal/handlers/transcode.go
var CanonicalLadder = []VideoRung{
    {1080, 5000, 192},   // height, video kbps, audio kbps
    {720,  2800, 128},
    {480,  1200,  96},
    {360,   600,  64},
}
// PickLadder(srcHeight) returns canonical rungs <= srcHeight, plus an
// extra "source-height" rung when source doesn't sit on a canonical row
// (e.g. 2160p source → adds {2160, ~20000, 192} on top).
```

Only qualities `≤ input.height` are emitted — never upscales. Output:

```
/storage/segments/{object-id}/
├── master.m3u8                  ← player loads this
├── 1080p/playlist.m3u8 + segment_NNN.ts
├── 720p/playlist.m3u8 + ...
├── 480p/playlist.m3u8 + ...
├── 360p/playlist.m3u8 + ...
└── thumb_0.jpg, thumb_1.jpg, thumb_2.jpg, thumb_3.jpg   (0%, 25%, 50%, 75% of duration)
```

---

## 4½. Go Cleaner (`s3-cleaner`)

**Image:** built from `api/Dockerfile` (same binary, `cleaner` target).
**Process:** ticks every `CLEANUP_INTERVAL_SECONDS` (default 30s) +
fsnotify reactive wakeups from filesystem events.

### Code layout

```
api/internal/cleaner/
├── cleaner.go     tick loop, run_record, audit log writer
├── shards.go      runShardSweep, walkShard, sweepBucketRoots (V4 adaptive Merkle trie)
├── orphans.go     reapOrphans (V2 bloom fallback), confirmAndReap (two-strike)
└── watcher.go     fsnotify subscriber → marks shards dirty in real time
```

### Five concurrent invariants enforced per tick

1. **Trash vacuum** — purge soft-deleted objects past `TRASH_RETENTION_DAYS`.
2. **Multipart sweep** — abort `multipart_uploads` rows past 7d expiry,
   reap their `tmp/{U}` dirs, refund quota.
3. **Stuck-transcode reaper** — restart `transcode_jobs` stuck in
   `processing` longer than `STUCK_TRANSCODE_MINUTES`.
4. **V4 shard sweep** — process splits, walk dirty leaves in parallel
   (`WalkConcurrency`), reconcile disk ⇄ DB per shard.
5. **V2 bloom fallback** — periodic full disk walk for paths the V4
   index doesn't track (segments, multipart tmp, bucket-root strays).

### Two-strike + age-gate

A path needs to show up as orphaned on **two consecutive ticks** before
being deleted. Closes "row exists but write is mid-flight" races. State
lives in `cleaner.orphanCandidates` (map keyed by path); pruned each
tick of paths that vanished on their own.

Files newer than `ORPHAN_MIN_AGE_MINUTES` (default 30) are exempt
regardless. With `ORPHAN_MIN_AGE_MINUTES=0` the worker bypasses the
age check (useful for tests; dangerous in prod — see
[production-tuning.md](../production-tuning.md)).

### Segments bloom is filtered

A `/storage/segments/{O}/` dir is considered legitimate only if the
owning object has `transcode_status IN ('done','pending','processing')`
or `transcoded_bytes > 0`. So a `skipped_quota` / `failed_quota` /
`none` object's segments dir (if one somehow exists) gets reaped.

### Bucket-root sweep

`sweepBucketRoots` runs every tick (NOT gated on dirty shards — that
was a fixed bug). It lists each non-archived bucket directly, skips
`objects/` and `tmp/`, and feeds anything else through the two-strike
machine. So a file dropped into `buckets/{B}/evil.sh` is detected
within ~60s.

---

## 5. Nginx (`s3-nginx`)

**Image:** `nginx:1.27-alpine`
**Port:** 8080 (host), proxies to internal services.
**Volumes:** read-only `/storage` bind-mount; read-only nginx.conf bind-mount.

### Three jobs

1. **Reverse proxy to API** at `/api/*` (strips prefix)
2. **Reverse proxy to dashboard** at `/`
3. **Static file server** for `/stream/{object-id}/...` via `sendfile()`

### Why `sendfile()` matters for HLS

A 720p HLS stream chunk-loads every ~6 seconds during playback. 1000 viewers =
1000 segment downloads/second. Each segment is ~2 MB. `sendfile()` is a kernel
syscall that pipes a file directly from disk into the network socket without
copying through user space. ~10× faster than streaming through the Go API.

### Cloudflare edge caching

`location ~ \.ts$` sends `Cache-Control: public, max-age=31536000, immutable`.
Segments are content-addressable (their filename is part of their object's
unique ID), so they're safe to cache forever. Cloudflare caches them at all
300+ POPs — your laptop only serves each segment once.

---

## 6. Next.js Dashboard (`s3-dashboard`)

**Image:** built from `dashboard/Dockerfile` (Node 22-alpine, standalone output).
**Port:** 3001 (internal; nginx fronts it).

### Code layout

```
dashboard/
├── app/                            App Router pages
│   ├── login/page.tsx              email+pass → JWT
│   └── dashboard/
│       ├── layout.tsx              auth guard, populates user store
│       ├── page.tsx                overview
│       ├── buckets/                file browser, upload, media preview
│       ├── keys/                   Bearer keys + S3 credentials
│       └── admin/                  users, audit, system, storage
├── components/
│   ├── upload-zone.tsx             drag/drop → multipart with 4× parallelism
│   ├── video-player.tsx            video.js wrapper for HLS playback
│   ├── quota-bar.tsx               green/yellow/red usage bar
│   └── nav.tsx                     sidebar; admin items if role=admin
└── lib/
    ├── api.ts                      fetch wrapper, JWT in localStorage
    ├── multipart.ts                XHR-based 5MiB-chunk uploader (XHR for progress)
    ├── user.ts                     module-scoped pub/sub for current user
    └── format.ts                   bytes/dates/file-type classifier
```

### Why XHR not fetch for uploads

`fetch()` doesn't expose upload progress events. We need them for the
progress bar, so each part upload uses `XMLHttpRequest` directly.

### Auth state

JWT lives in `localStorage` under key `ps3_token`. Survives page reloads.
Cleared on logout. No cookies → no CSRF concern.

---

## 7. cloudflared (optional)

**Image:** `cloudflare/cloudflared:latest`

Two opt-in modes via Docker Compose profiles. See
[../cloudflare-tunnel.md](../cloudflare-tunnel.md) for setup.

Cloudflared opens an **outbound** HTTP/2 connection to Cloudflare's edge and
keeps it open. Incoming requests to your hostname get tunneled in over that
existing connection. Your firewall doesn't need any open ports.

---

## See also

- [data-flows.md](./data-flows.md) — what happens when you upload a video
- [database-schema.md](./database-schema.md) — every table, every column
