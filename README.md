# PersonalS3

A self-hosted S3-compatible object storage system with built-in media transcoding.
Runs on your laptop, globally accessible via Cloudflare Tunnel.

> **Full documentation** → [`docs/`](./docs/README.md) — architecture diagrams,
> setup guides, client cookbooks (curl / aws-cli / boto3 / rclone), API
> reference, ops, troubleshooting.

See [plan-1.md](./plan-1.md) for the original architecture spec.

---

## Status

The project is being built in 10 parts. Currently completed:

- [x] **Part 1** — Foundation (Docker Compose, PostgreSQL, Valkey)
- [x] **Part 2** — Go API Core (buckets, objects)
- [x] **Part 3** — Auth + Quotas + Rate limiting + Audit log
- [x] **Part 4** — Multipart upload
- [x] **Part 5** — Transcoding pipeline (FFmpeg)
- [x] **Part 6** — Streaming serving (Nginx static)
- [x] **Part 7** — Next.js dashboard
- [x] **Part 8** — Admin panel
- [x] **Part 9** — Cloudflare Tunnel
- [x] **Part 10** — AWS CLI / SDK compatibility

---

## Quick Start (Part 1)

Prerequisites:
- Docker 20+ with Compose v2
- ~1GB free disk (data layer only at this phase)

```bash
# 1. Generate fresh secrets (interactive, asks for admin email + password)
./scripts/init-secrets.sh
# (or, manually: cp .env.example .env && edit it)

# 2. Start the data layer
docker compose up -d

# 3. Verify both services are healthy
docker compose ps

# 4. Connect to PostgreSQL and inspect tables
docker compose exec postgres psql -U s3admin -d personals3 -c "\dt"

# 5. Verify the seed admin user exists
docker compose exec postgres psql -U s3admin -d personals3 \
  -c "SELECT email, role, quota_bytes FROM users;"

# 6. Verify Valkey is reachable
docker compose exec valkey valkey-cli -a "$(grep VALKEY_PASSWORD .env | cut -d= -f2)" ping
# → PONG
```

### Tear down

```bash
# Stop containers (data persists in volumes)
docker compose down

# Stop AND delete all data (full reset)
docker compose down -v
```

### Reset database to fresh state

```bash
docker compose down -v
docker compose up -d
```

---

## Part 2: Running the Go API

The API service is now in `docker-compose.yml`. The same `docker compose up -d`
will build the Go binary in a container (you don't need Go installed locally) and
start it alongside PostgreSQL and Valkey.

```bash
# Build + start everything (first run takes ~2 min for Go build)
docker compose up -d --build

# Tail API logs
docker compose logs -f api

# Run the Part 2 smoke test (creates a bucket, uploads, downloads, deletes)
./scripts/test-part2.sh
```

### What works in Part 2

```
Buckets:   PUT /:bucket    HEAD /:bucket   DELETE /:bucket   GET /
Objects:   PUT /:bucket/*  GET /:bucket/*  HEAD /:bucket/*   DELETE /:bucket/*
List:      GET /:bucket?prefix=...&max-keys=...
Health:    GET /healthz
```

### Manual examples

```bash
# Create a bucket
curl -X PUT http://localhost:3000/photos

# Upload a file (key can contain slashes — they are part of the key, not directories)
curl -X PUT http://localhost:3000/photos/holidays/beach.jpg \
     -H "Content-Type: image/jpeg" \
     --data-binary @beach.jpg

# Download it
curl http://localhost:3000/photos/holidays/beach.jpg -o downloaded.jpg

# List
curl http://localhost:3000/photos
curl 'http://localhost:3000/photos?prefix=holidays/'

# Delete
curl -X DELETE http://localhost:3000/photos/holidays/beach.jpg
curl -X DELETE http://localhost:3000/photos
```

---

## Part 3: Auth + Quotas + Rate Limiting + Audit Log

All storage operations now require authentication. Two credential types:

| Type | Where used | How to get |
|---|---|---|
| **JWT** (`Authorization: Bearer eyJ...`) | Dashboard login (24h expiry) | `POST /auth/login` |
| **API key** (`Authorization: Bearer psk_xxx.yyy`) | API clients, CLI, SDKs | `POST /auth/keys` (after JWT login) |

### Login → get API key → use it

```bash
# 1. Login (seeded admin: admin@local / admin)
JWT=$(curl -s -X POST http://localhost:3000/auth/login \
       -H "Content-Type: application/json" \
       -d '{"email":"admin@local","password":"admin"}' \
     | python3 -c "import json,sys;print(json.load(sys.stdin)['token'])")

# 2. Create an API key (returned ONCE, in plaintext)
KEY=$(curl -s -X POST http://localhost:3000/auth/keys \
       -H "Authorization: Bearer $JWT" \
       -H "Content-Type: application/json" \
       -d '{"name":"my-laptop"}' \
     | python3 -c "import json,sys;print(json.load(sys.stdin)['key'])")
echo "$KEY"   # psk_abc12345.LongRandomSecret...
                                  # save this somewhere safe

# 3. Use the API key for everything else
curl -X PUT http://localhost:3000/photos \
     -H "Authorization: Bearer $KEY"

curl -X PUT http://localhost:3000/photos/cat.jpg \
     -H "Authorization: Bearer $KEY" \
     --data-binary @cat.jpg
```

### What's enforced

- **Auth required** — every endpoint except `/healthz` and `/auth/login` returns 401 without a Bearer token
- **Atomic quota** — `PUT` reserves bytes against your quota; if you'd exceed it, you get 507 *Insufficient Storage*
- **Quota refund on DELETE** — usage decreases automatically
- **Rate limit** — 1000 req/min per user (10000 for admins), Valkey-backed. Returns 429 + `Retry-After: 60`
- **Audit log** — every authenticated request is written to `audit_log` table asynchronously (no latency impact)
- **Admin role** — `AdminOnly` middleware available for Part 8 admin endpoints

### Verify everything works

> ⚠️ The seed migration was updated in Part 3 (uses `pgcrypto.crypt()` instead of a hardcoded bcrypt hash). If you ran Part 2 already, wipe the data layer first:

```bash
docker compose down -v
docker compose up -d --build
./scripts/test-part3.sh
```

### Authenticated endpoints (current state)

```
Public:
  GET    /healthz
  POST   /auth/login            { email, password }  → { token, user }

Authenticated (requires Bearer JWT or Bearer API key):
  GET    /auth/me               → current user, quota, usage
  GET    /auth/keys             → list your API keys (no secrets)
  POST   /auth/keys             { name?, expiresAt? } → { key, ... }
  DELETE /auth/keys/:id         → revoke a key

  GET    /                      → list your buckets
  PUT    /:bucket               → create bucket
  HEAD   /:bucket               → bucket exists?
  DELETE /:bucket               → delete (must be empty)
  GET    /:bucket               → list objects (?prefix=&max-keys=)

  PUT    /:bucket/*             → upload object  (enforces quota)
  GET    /:bucket/*             → download (Range requests supported)
  HEAD   /:bucket/*             → metadata only
  DELETE /:bucket/*             → delete (refunds quota)
```

---

## Part 4: Multipart Upload

For files >5 MiB, split into parts client-side and upload in parallel.
The S3 multipart API is implemented over the same routes — query strings select the operation:

```
POST   /:bucket/*?uploads                    → initiate, returns {uploadId}
PUT    /:bucket/*?partNumber=N&uploadId=X    → upload part N, returns ETag
GET    /:bucket/*?uploadId=X                 → list parts (for resumable uploads)
POST   /:bucket/*?uploadId=X                 → complete (body: {parts:[{partNumber,etag},...]})
DELETE /:bucket/*?uploadId=X                 → abort, refunds reserved quota
```

Same path with no query string falls through to the single-object operations.

### Rules

- Part numbers 1-10000
- Minimum part size 5 MiB (except the last one) — enforced on `Complete`
- Max object size 5 TB (10000 parts × 5 GB max each)
- ETag for the final object follows S3 format: `<md5-of-md5s>-<N>`, e.g. `a1b2c3...-42`
- Quota is reserved per-part as parts arrive. `Abort` refunds. `Complete` keeps.
- In-progress uploads expire after 7 days (cleanup cron is a future TODO)

### Test it

```bash
./scripts/test-part4.sh
```

This creates a 16 MiB random file, splits it into 3×5MiB + 1×1MiB parts,
uploads them, completes, downloads, and verifies MD5 matches.

---

## Part 5: Transcoding Pipeline (Python + FFmpeg)

When you upload a video, audio, or image file, a `transcode_jobs` row is
inserted. The Python worker (one or more containers) polls the table
using `SELECT ... FOR UPDATE SKIP LOCKED`, claims a job, and runs FFmpeg.

### Outputs by file type

**Video** (mp4/mov/mkv/avi/webm/...) → `/storage/segments/{object-id}/`
```
master.m3u8                       HLS master playlist (player loads this)
1080p/playlist.m3u8 + *.ts        only included if input is ≥1080p
720p/playlist.m3u8 + *.ts         5MB/s segments
480p/playlist.m3u8 + *.ts         2.5MB/s
360p/playlist.m3u8 + *.ts         1MB/s
thumb_0.jpg, thumb_1.jpg, ...     frames at 0%, 25%, 50%, 75%
```

**Audio** (mp3/wav/flac/aac/...) → `/storage/segments/{object-id}/`
```
audio.m3u8 + audio_*.aac          HLS audio (browser streaming)
audio_320.mp3                     universal compat
audio.ogg                         open Vorbis
```

**Image** (png/jpg/gif/tiff/...) → `/storage/segments/{object-id}/`
```
original.webp                     quality 85, EXIF-rotated
original.avif                     AV1 still image (smaller)
thumb_100.webp, thumb_300.webp, thumb_800.webp
```

### Track progress

```bash
# What's pending/processing/done?
docker compose exec postgres psql -U s3admin -d personals3 \
  -c "SELECT status, file_type, attempts, error FROM transcode_jobs ORDER BY created_at DESC LIMIT 10;"

# Worker logs
docker compose logs -f worker
```

### Scale workers (CPU-bound — set to your CPU count)

```bash
docker compose up -d --scale worker=4
```

### Test it

```bash
./scripts/test-part5.sh
```

Generates a 5-second test pattern video in the worker container, uploads it,
polls until `transcode_status=done`, then verifies HLS playlists + segments
exist on disk. Also tests image transcoding.

---

## Part 6: Nginx — Streaming + Reverse Proxy

Nginx now sits in front of everything on port **8080** (set in `.env`).

```
┌──── client (browser/CLI) ────┐
│           ▼                  │
│   localhost:8080             │
│                              │
│   /stream/{id}/...   →  static file from /storage/segments/{id}/...
│                          ├── .m3u8 → application/vnd.apple.mpegurl
│                          │           Cache: max-age=60 (playlists may update)
│                          ├── .ts   → video/mp2t
│                          │           Cache: max-age=31536000, immutable
│                          ├── .webp/.avif/.jpg → image MIMEs, 30d cache
│                          └── CORS: Access-Control-Allow-Origin: *
│
│   /nginx-health      →  "ok" (nginx-only liveness)
│
│   everything else    →  proxy_pass http://api:3000
│                         (PUT, multipart, auth, listing — all proxied)
└──────────────────────────────┘
```

### Why a separate /stream/ path?

- The Go API requires auth on every route → HLS players can't send Bearer headers per segment
- Nginx serves the static files directly via `sendfile()` (kernel-to-socket zero-copy → ~10x faster than going through Go)
- Long cache headers let Cloudflare's edge cache serve segments globally without hitting your laptop
- Object IDs are UUIDs → unguessable → "security through obscurity is fine for media URLs you've published"

### Test it

```bash
./scripts/test-part6.sh
```

Then load the master.m3u8 URL it prints in a browser-side HLS player (e.g. `https://hls-js.netlify.app/demo/?src=<URL>`) to see the video play with quality switching.

### Important: from this point on, hit nginx (port 8080) not the API (port 3000)

Earlier test scripts hit `localhost:3000` directly. They still work, but real
clients should go through nginx for proper streaming. Update your scripts:

```bash
export API_URL=http://localhost:8080
./scripts/test-part3.sh   # still passes
```

---

## Part 7: Next.js Dashboard

A web UI for the whole system. Runs at **http://localhost:3001**.

### Pages

| Route | What |
|---|---|
| `/login` | email + password → JWT in localStorage |
| `/dashboard` | overview: quota bar, bucket count |
| `/dashboard/buckets` | list/create/delete buckets |
| `/dashboard/buckets/[name]` | file browser, drag-drop upload, embedded media player |
| `/dashboard/keys` | create / revoke API keys |

### Features

- **Drag-and-drop upload** with progress bar — files > 8 MiB use multipart with 4 concurrent 5 MiB chunks
- **HLS playback** for transcoded videos via `video.js` (adaptive bitrate, picks quality automatically)
- **Image previews** use the transcoded WebP (faster, smaller); falls back to original if transcoding hasn't finished
- **Audio** plays the HLS stream
- **Transcode status indicator** — `pending` / `processing` / `done` / `failed` visible per object
- **API key management** with one-time-display new-key panel + copy button

### Verify it works

```bash
docker compose up -d --build
```

Wait ~2 min for the Next.js build. Then open **http://localhost:3001** and log in as `admin@local / admin`.

Try:
1. Create a bucket
2. Drag a video file into the upload zone (>8 MiB to exercise multipart)
3. Watch the quota bar update
4. Click the uploaded file — embedded player appears once transcoding completes (~10s for short clips)

---

## Part 9: Cloudflare Tunnel — Global Access

Make your laptop's storage reachable from anywhere on the internet —
no static IP, no port forwarding, no exposed firewall, free TLS certificates.

Two opt-in modes via Docker Compose profiles. See [cloudflared/README.md](./cloudflared/README.md) for full details.

### Quick start (no Cloudflare account needed)

```bash
docker compose --profile quick-tunnel up -d cloudflared-quick
docker compose logs cloudflared-quick | grep trycloudflare.com
# →  https://<random>.trycloudflare.com
```

Open the printed URL anywhere in the world — your dashboard + API + HLS streaming all work.

> The URL changes on every restart. Use this for demos.

### Permanent custom domain

If you own a domain and have it on Cloudflare nameservers:

```bash
# 1. Get a tunnel token from dash.cloudflare.com → Zero Trust → Networks → Tunnels
# 2. Configure public hostnames there (s3.yourdomain.com → http://nginx:80)
# 3. Save the token to .env: CLOUDFLARED_TOKEN=eyJ...
docker compose --profile tunnel up -d cloudflared
```

Your stack is now permanently at `https://s3.yourdomain.com`.

### Architecture (what happens to a request)

```
Anyone on the internet
  ↓ HTTPS (TLS certs auto-managed by Cloudflare)
Cloudflare edge (DDoS protection, caching of /stream/*.ts segments globally)
  ↓ encrypted persistent connection (initiated outbound by cloudflared)
cloudflared in Docker on your laptop
  ↓ HTTP via Docker network
nginx (sendfile for HLS, proxy to API for everything else)
  ↓
api → postgres → /storage on your 1 TB disk
```

Your home IP is hidden, port 80/443 on your router stays closed,
and HLS segments cache at the edge — viewers in Asia get them from
Cloudflare's nearest PoP rather than your laptop.

---

## Part 10: AWS CLI / SDK Compatibility (SigV4)

You can drive PersonalS3 from any S3-compatible client: `aws-cli`, `boto3`,
`aws-sdk-js`, `rclone`, `s3cmd`, `Cyberduck`, etc.

### 1. Create S3 credentials

Dashboard → **Credentials** → **S3 Credentials** → **Create**.
You get an Access Key ID + Secret Access Key (shown ONCE).

Or via API:
```bash
curl -X POST http://localhost:8080/api/auth/s3-credentials \
  -H "Authorization: Bearer $JWT" \
  -H "Content-Type: application/json" \
  -d '{"name":"my-laptop"}'
```

### 2. Configure aws-cli

```bash
aws configure --profile personals3
# AWS Access Key ID:     AKIA...
# AWS Secret Access Key: <secret>
# Default region name:   us-east-1
# Default output:        json
```

### 3. Use it

```bash
ENDPOINT="http://localhost:8080/api"

# List buckets
aws --profile personals3 --endpoint-url=$ENDPOINT s3 ls

# Create a bucket
aws --profile personals3 --endpoint-url=$ENDPOINT s3 mb s3://photos

# Upload a file
aws --profile personals3 --endpoint-url=$ENDPOINT s3 cp ~/cat.jpg s3://photos/

# Sync a whole directory
aws --profile personals3 --endpoint-url=$ENDPOINT s3 sync ~/Pictures s3://photos/

# Download
aws --profile personals3 --endpoint-url=$ENDPOINT s3 cp s3://photos/cat.jpg ./
```

### Same idea from Python (boto3):

```python
import boto3
s3 = boto3.client('s3',
    endpoint_url='http://localhost:8080/api',
    aws_access_key_id='AKIA...',
    aws_secret_access_key='your-secret',
    region_name='us-east-1',
)
s3.upload_file('/tmp/big.zip', 'photos', 'archives/big.zip')
```

### What's compatible

| S3 feature | Supported? |
|---|---|
| ListBuckets, CreateBucket, DeleteBucket | ✅ |
| ListObjectsV2 (with `prefix`, `max-keys`) | ✅ |
| PutObject (single-part ≤5 GB) | ✅ |
| GetObject (with Range requests) | ✅ |
| HeadObject, DeleteObject | ✅ |
| Multipart upload (Initiate/UploadPart/Complete/Abort) | ✅ |
| Versioning, lifecycle rules | ❌ (planned) |
| Presigned URLs | ❌ (planned) |
| Bucket policies, ACLs | ❌ (we have per-user quotas instead) |
| Server-side encryption | ❌ (data is on your disk; use full-disk encryption) |
| S3 Select | ❌ |

### Test it

Requires `aws` CLI installed locally:

```bash
./scripts/test-part10.sh
```

Goes through: list, create bucket, upload (single + multipart), download,
diff, delete, cleanup.

---

## Architecture overview (final)

```
                ┌────────────────────────────────────┐
                │  Browser / aws-cli / mobile / curl │
                └────────────────┬───────────────────┘
                                 │ HTTPS
                ┌────────────────▼───────────────────┐
                │       Cloudflare edge (Part 9)     │
                │   • TLS termination                │
                │   • DDoS, caching of /stream/*.ts  │
                └────────────────┬───────────────────┘
                                 │ encrypted tunnel
                ┌────────────────▼───────────────────┐
                │       cloudflared (Docker)         │
                └────────────────┬───────────────────┘
                                 │ HTTP
                ┌────────────────▼───────────────────┐
                │      nginx (port 8080)             │
                │   • /              → dashboard     │
                │   • /api/*         → API           │
                │   • /stream/*      → static (sendfile)
                └─┬──────────────┬───────────────┬──┘
                  │              │               │
        ┌─────────▼─────┐  ┌─────▼────┐  ┌─────▼────────┐
        │  Next.js 15   │  │  Go API  │  │  storage/    │
        │  dashboard    │  │  (chi)   │  │  (1 TB local)│
        └───────────────┘  └────┬─────┘  └──────────────┘
                                │ pgx
                ┌───────────────▼──────────┐
                │  PostgreSQL + Valkey     │
                │  (metadata, quotas,      │
                │   rate-limits, jobs)     │
                └───────────────┬──────────┘
                                │ SKIP LOCKED
                ┌───────────────▼──────────┐
                │  Python worker(s)        │
                │  + FFmpeg + Pillow       │
                │  → HLS / WebP / AVIF     │
                └──────────────────────────┘
```

## Verified test suite

```bash
./scripts/test-part3.sh    # auth, quotas, rate limiting, audit log
./scripts/test-part4.sh    # multipart upload (16 MiB → 4 parts → MD5 verified)
./scripts/test-part5.sh    # video + image transcoding pipeline
./scripts/test-part6.sh    # nginx HLS streaming (CORS, cache, segments)
./scripts/test-part10.sh   # AWS CLI compatibility (SigV4)
```

---

## Database Schema (Part 1)

Migrations run automatically on first startup from `db/migrations/`.

Tables created:
- `users` — user accounts with quotas
- `api_keys` — bcrypt-hashed API keys per user
- `buckets` — S3-compatible bucket namespaces
- `objects` — uploaded objects with metadata
- `multipart_uploads` — in-progress multipart upload state
- `transcode_jobs` — queue table polled by Python workers
- `audit_log` — append-only event log

To inspect:
```bash
docker compose exec postgres psql -U s3admin -d personals3
\dt              -- list all tables
\d objects       -- describe objects table
\q               -- quit
```

---

## Project Structure

```
S3orSimilar/
├── docker-compose.yml         ← orchestrates all services
├── .env / .env.example        ← secrets and config
├── plan-1.md                  ← full architecture spec
├── README.md                  ← this file
│
├── api/                       ← Go API server (Part 2+)
├── worker/                    ← Python transcoder (Part 5)
├── dashboard/                 ← Next.js dashboard (Part 7)
├── nginx/                     ← Reverse proxy (Part 6, 9)
├── cloudflared/               ← Tunnel config (Part 9)
│
├── db/migrations/             ← PostgreSQL schema
│   ├── 001_initial.sql
│   └── 002_seed.sql
│
└── storage/                   ← Object data (created at runtime)
    ├── buckets/{bucket-id}/   ← per-bucket object data
    └── segments/{object-id}/  ← HLS segments (Part 5)
```
