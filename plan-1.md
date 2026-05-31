# Plan-1: PersonalS3 — S3-Compatible Object Storage on Your Laptop

---

## TABLE OF CONTENTS

1. [System Overview](#1-system-overview)
2. [High-Level Architecture](#2-high-level-architecture)
3. [Low-Level Component Breakdown](#3-low-level-component-breakdown)
   - 3.1 [Storage Engine](#31-storage-engine-filesystem-layer)
   - 3.2 [Metadata Database (PostgreSQL)](#32-metadata-database-postgresql)
   - 3.3 [API Server (Fastify + Node.js)](#33-api-server-fastify--nodejs)
   - 3.4 [Dashboard (Next.js)](#34-dashboard-nextjs)
   - 3.5 [Redis](#35-redis-state--rate-limiting)
   - 3.6 [Nginx](#36-nginx-local-reverse-proxy)
   - 3.7 [Cloudflare Tunnel](#37-cloudflare-tunnel-global-access)
4. [Full Data Flow: End-to-End Upload](#4-full-data-flow-end-to-end-upload)
5. [What You Can Do With This System](#5-what-you-can-do-with-this-system)
6. [Tech Stack Summary](#6-tech-stack-summary)
7. [Build Order](#7-build-order)
8. [AWS S3: Complete Deep Dive](#8-aws-s3-complete-deep-dive)
   - 8.1 [What S3 Actually Is](#81-what-s3-actually-is)
   - 8.2 [Core Concepts](#82-core-concepts)
   - 8.3 [Storage Classes](#83-storage-classes)
   - 8.4 [Object Lifecycle](#84-object-lifecycle)
   - 8.5 [Versioning](#85-versioning)
   - 8.6 [Access Control](#86-access-control)
   - 8.7 [S3 API: Complete Reference](#87-s3-api-complete-reference)
   - 8.8 [Multipart Upload (Deep Dive)](#88-multipart-upload-deep-dive)
   - 8.9 [Presigned URLs](#89-presigned-urls)
   - 8.10 [S3 Events & Notifications](#810-s3-events--notifications)
   - 8.11 [Static Website Hosting](#811-static-website-hosting)
   - 8.12 [Replication](#812-replication)
   - 8.13 [S3 Select & Glacier Select](#813-s3-select--glacier-select)
   - 8.14 [Transfer Acceleration](#814-transfer-acceleration)
   - 8.15 [Inventory, Analytics, Metrics](#815-inventory-analytics-metrics)
   - 8.16 [Object Lock & Compliance](#816-object-lock--compliance)
   - 8.17 [Intelligent Tiering](#817-intelligent-tiering)
   - 8.18 [Using S3 with AWS CLI](#818-using-s3-with-aws-cli)
   - 8.19 [Using S3 with SDKs](#819-using-s3-with-sdks)
9. [Coverage Map: What Plan-1 Covers vs What S3 Has](#9-coverage-map-what-plan-1-covers-vs-what-s3-has)

---

## 1. System Overview

A self-hosted, S3-compatible object storage system running on a personal 1TB laptop.
Globally accessible via Cloudflare Tunnel — no static IP, no port forwarding, no cloud bills.

**Goals:**
- Upload and download files from anywhere in the world
- Allocate storage quotas to different users (like giving someone 5GB)
- Dashboard showing per-user usage, bucket contents, and audit logs
- Multipart/chunked upload for large files
- API compatible enough with S3 that standard clients (AWS CLI, SDKs) can use it

---

## 2. High-Level Architecture

```
                            INTERNET
                               │
                    ┌──────────▼──────────┐
                    │   Cloudflare DNS    │
                    │  (your-domain.com)  │
                    └──────────┬──────────┘
                               │ HTTPS (encrypted, TLS from Cloudflare)
                    ┌──────────▼──────────┐
                    │  Cloudflare Tunnel  │  ← no port forwarding needed
                    │   (cloudflared)     │    your home IP stays hidden
                    └──────────┬──────────┘
                               │ HTTP (localhost only)
                    ┌──────────▼──────────┐
                    │       Nginx         │  ← reverse proxy + routing
                    │  (local port 80)    │    routes by subdomain
                    └────┬──────────┬─────┘
                         │          │
          ┌──────────────▼──┐  ┌───▼──────────────┐
          │   API Server    │  │    Dashboard      │
          │  Fastify/Node   │  │    Next.js        │
          │  :3000          │  │    :3001          │
          └────────┬────────┘  └────────┬──────────┘
                   │                    │
          ┌────────▼────────────────────▼──────────┐
          │              PostgreSQL                │
          │    (metadata: users, buckets,          │
          │     objects, quotas, audit log)        │
          └────────────────┬───────────────────────┘
                           │
          ┌────────────────▼───────────────────────┐
          │               Redis                    │
          │    (sessions, multipart state,         │
          │     rate-limit counters)               │
          └────────────────┬───────────────────────┘
                           │
          ┌────────────────▼───────────────────────┐
          │          Storage Engine                │
          │   /storage/buckets/{id}/{key}          │
          │   (your 1TB local filesystem)          │
          └────────────────────────────────────────┘
```

**Traffic path (upload):**
```
Your phone (anywhere in world)
  → Cloudflare edge (TLS termination)
    → cloudflared tunnel (encrypted WireGuard)
      → Nginx (localhost:80)
        → Fastify API (localhost:3000)
          → Auth check → Quota check
            → Write to /storage/
              → Update PostgreSQL metadata
                → 200 OK back up the chain
```

---

## 3. Low-Level Component Breakdown

### 3.1 Storage Engine (Filesystem Layer)

**Purpose:** Physically stores raw file bytes on your 1TB disk.

**Directory layout:**
```
/storage/
├── buckets/
│   └── {bucket-uuid}/
│       ├── objects/
│       │   └── {sha256-of-key}/
│       │       ├── data              ← actual file bytes
│       │       └── meta.json        ← content-type, etag, size
│       └── tmp/
│           └── {upload-id}/         ← multipart in-progress
│               ├── part-0001
│               ├── part-0002
│               └── part-0003
└── config/
    └── server.json
```

**Why SHA256 of key as folder name?**
- Keys like `photos/2024/holiday/img.jpg` have slashes — using the hash avoids nested directory creation
- Deterministic: given a key, you can always find its path without a DB lookup
- Collision-resistant: two different keys never map to the same folder

**Chunking rules:**
| File size | Strategy |
|---|---|
| < 5 MB | Single write to `data` file |
| ≥ 5 MB | Write 5MB chunks as `part-XXXX`, then concatenate on complete |

**ETag calculation (S3-compatible):**
```
single upload:  ETag = MD5(file_bytes)
multipart:      ETag = MD5(MD5(part1) + MD5(part2) + ...)-{num_parts}
                e.g.  "d41d8cd98f00b204e9800998ecf8427e-42"
```

**Storage math for your 1TB disk:**
```
Raw capacity:        1,000 GB
OS + software:         ~50 GB
Database + logs:        ~5 GB
Available for files:  ~945 GB
```

---

### 3.2 Metadata Database (PostgreSQL)

**Purpose:** Tracks all metadata — who owns what, how big, when, quotas.

**Why PostgreSQL over SQLite?**
- ACID transactions: quota accounting must be atomic (no double-spending storage)
- Concurrent writes: multiple users uploading simultaneously
- JSON columns: flexible part tracking in multipart uploads
- Row-level locking: safe quota updates

**Full schema:**

```sql
-- Enable UUID generation
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Users / tenants
CREATE TABLE users (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email        TEXT UNIQUE NOT NULL,
  name         TEXT NOT NULL,
  quota_bytes  BIGINT NOT NULL DEFAULT 10737418240,  -- 10 GB default
  used_bytes   BIGINT NOT NULL DEFAULT 0,
  api_key      TEXT UNIQUE NOT NULL,                 -- bcrypt hashed
  role         TEXT NOT NULL DEFAULT 'user'          -- 'admin' | 'user'
                 CHECK (role IN ('admin', 'user')),
  is_active    BOOLEAN DEFAULT true,
  created_at   TIMESTAMPTZ DEFAULT now(),
  updated_at   TIMESTAMPTZ DEFAULT now()
);

-- Buckets (namespaces for objects)
CREATE TABLE buckets (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name             TEXT NOT NULL,
  owner_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  is_public        BOOLEAN DEFAULT false,
  versioning       BOOLEAN DEFAULT false,
  created_at       TIMESTAMPTZ DEFAULT now(),
  UNIQUE(owner_id, name)
);

-- Objects (individual files)
CREATE TABLE objects (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  bucket_id     UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  key           TEXT NOT NULL,               -- "photos/2024/img.jpg"
  version_id    TEXT,                        -- for versioning (future)
  size_bytes    BIGINT NOT NULL,
  etag          TEXT NOT NULL,
  content_type  TEXT NOT NULL DEFAULT 'application/octet-stream',
  storage_path  TEXT NOT NULL,               -- absolute path on disk
  metadata      JSONB DEFAULT '{}',          -- user-defined headers
  is_deleted    BOOLEAN DEFAULT false,       -- soft delete for versioning
  created_at    TIMESTAMPTZ DEFAULT now(),
  updated_at    TIMESTAMPTZ DEFAULT now(),
  UNIQUE(bucket_id, key)
);

-- In-progress multipart uploads
CREATE TABLE multipart_uploads (
  upload_id    TEXT PRIMARY KEY,             -- UUID generated at initiation
  bucket_id    UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  key          TEXT NOT NULL,
  owner_id     UUID NOT NULL REFERENCES users(id),
  parts        JSONB DEFAULT '[]',           -- [{partNumber, etag, size, path}]
  total_size   BIGINT DEFAULT 0,             -- running total as parts arrive
  status       TEXT DEFAULT 'in-progress'
                 CHECK (status IN ('in-progress', 'complete', 'aborted')),
  created_at   TIMESTAMPTZ DEFAULT now(),
  expires_at   TIMESTAMPTZ DEFAULT now() + INTERVAL '7 days'
);

-- Audit log (immutable event stream)
CREATE TABLE audit_log (
  id          BIGSERIAL PRIMARY KEY,
  user_id     UUID REFERENCES users(id),
  action      TEXT NOT NULL,                 -- 'CREATE_BUCKET' | 'PUT_OBJECT' | 'GET_OBJECT' | 'DELETE_OBJECT'
  bucket_name TEXT,
  object_key  TEXT,
  size_bytes  BIGINT,
  status_code INT,
  ip_address  TEXT,
  user_agent  TEXT,
  ts          TIMESTAMPTZ DEFAULT now()
);

-- Indexes for performance
CREATE INDEX idx_objects_bucket_key    ON objects(bucket_id, key);
CREATE INDEX idx_objects_key_prefix    ON objects(bucket_id, key text_pattern_ops); -- for prefix listing
CREATE INDEX idx_audit_log_user_ts     ON audit_log(user_id, ts DESC);
CREATE INDEX idx_multipart_expires     ON multipart_uploads(expires_at) WHERE status = 'in-progress';
```

**Quota enforcement (atomic):**
```sql
-- When uploading, atomically check and reserve quota
UPDATE users
SET used_bytes = used_bytes + $new_file_size
WHERE id = $user_id
  AND (used_bytes + $new_file_size) <= quota_bytes
RETURNING id;
-- If 0 rows returned → quota exceeded → reject upload
```

---

### 3.3 API Server (Fastify + Node.js)

**Purpose:** The core HTTP API — handles all storage operations.

**Why Fastify over Express?**
- 2–3x faster than Express on benchmarks
- Built-in schema validation (JSON Schema)
- Native streaming support — critical for large file uploads/downloads
- Plugin architecture keeps code organized

**Complete endpoint list:**

```
─── Authentication ──────────────────────────────────────────────────────────
POST   /auth/login                  → returns session token
POST   /auth/keys                   → generate new API key
DELETE /auth/keys/:keyId            → revoke API key
GET    /auth/keys                   → list your API keys

─── Bucket Operations ────────────────────────────────────────────────────────
GET    /                            → list all your buckets
PUT    /:bucket                     → create bucket
DELETE /:bucket                     → delete bucket (must be empty)
HEAD   /:bucket                     → check if bucket exists

─── Object Operations ────────────────────────────────────────────────────────
PUT    /:bucket/:key                → upload object (single-part, ≤5GB)
GET    /:bucket/:key                → download object (with streaming)
DELETE /:bucket/:key                → delete object
HEAD   /:bucket/:key                → get object metadata only
COPY   /:bucket/:key                → copy object (X-Amz-Copy-Source header)
GET    /:bucket                     → list objects (prefix, delimiter, max-keys, marker)

─── Multipart Upload ─────────────────────────────────────────────────────────
POST   /:bucket/:key?uploads        → initiate multipart upload
PUT    /:bucket/:key                → upload one part (?partNumber=N&uploadId=X)
POST   /:bucket/:key?uploadId=X     → complete multipart upload
DELETE /:bucket/:key?uploadId=X     → abort multipart upload
GET    /:bucket/:key?uploadId=X     → list uploaded parts

─── Admin (role: admin only) ─────────────────────────────────────────────────
GET    /admin/users                 → list all users + quotas + usage
POST   /admin/users                 → create new user
GET    /admin/users/:id             → get user details
PATCH  /admin/users/:id/quota       → update quota
DELETE /admin/users/:id             → deactivate user
GET    /admin/stats                 → disk usage, total objects, req/s, errors
GET    /admin/audit                 → audit log with filters (user, date, action)
```

**Middleware stack (executed in order on every request):**
```
Incoming Request
  │
  ▼
1. Request Logger        → logs method, path, IP, timestamp
  │
  ▼
2. Rate Limiter          → Redis: max 1000 req/min per API key, 10000 req/min admin
  │
  ▼
3. Auth Middleware        → parse Authorization header → verify API key → attach user to request
  │
  ▼
4. Route Handler
   ├── On PUT (upload):
   │    ├── Quota Pre-check    → reject before writing if over limit
   │    ├── Write to disk
   │    └── Quota Post-update  → atomic SQL UPDATE
   └── On GET/DELETE/etc:
        └── Ownership check   → user can only access their own buckets
  │
  ▼
5. Audit Logger          → write to audit_log table (async, non-blocking)
  │
  ▼
Response
```

**Streaming upload (key for large files, no memory buffering):**
```
Client sends 2GB file
  → Fastify receives it as a Node.js Readable stream
    → Pipe directly to fs.createWriteStream('/storage/...')
      → Never loads entire file into RAM
        → Memory usage stays flat at ~10MB regardless of file size
```

---

### 3.4 Dashboard (Next.js)

**Purpose:** Web UI for managing the system — no CLI needed for everyday use.

**Page structure:**
```
/                          → redirect to /login
/login                     → email + password → JWT cookie
/dashboard
  /dashboard/overview      → disk usage gauge, object count, req/s graph,
                             recent activity feed
  /dashboard/buckets       → list buckets with size + object count
  /dashboard/buckets/:name → file browser
                             ├── breadcrumb navigation (folder simulation)
                             ├── file list (name, size, type, date)
                             ├── drag-and-drop upload zone
                             ├── download / delete / copy-link per file
                             └── quota bar [███████░░░] 7.2 GB / 10 GB
  /dashboard/users         → admin only
                             ├── table: email, quota, used, %
                             ├── create user button
                             └── click user → set quota, revoke key
  /dashboard/audit         → filterable log: user, action, date range
  /dashboard/settings      → your API keys, regenerate, copy
```

**Key UI interactions:**

1. **Drag-and-drop upload with multipart progress:**
   ```
   File dropped → check size
     < 5MB  → single PUT request → progress bar jumps to 100%
     ≥ 5MB  → split into 5MB chunks in browser (File.slice())
              → POST ?uploads (get uploadId)
              → PUT chunks in parallel (4 concurrent)
              → POST complete
              → progress bar updates per chunk
   ```

2. **Quota visualization:**
   ```
   [████████████░░░░░░░░] 6.1 GB / 10 GB (61%)
    Green < 70%   Yellow 70-90%   Red > 90%
   ```

3. **Object browser simulating folders:**
   ```
   S3 has no real folders — keys like "a/b/c.jpg" create the illusion.
   Dashboard groups by common prefix up to the first "/" delimiter.
   Clicking "photos/" filters objects with prefix "photos/".
   ```

---

### 3.5 Redis (State & Rate Limiting)

**Purpose:** Millisecond-latency ephemeral state — not for permanent storage.

**Key schema:**
```
Key pattern                           Type    TTL     Purpose
─────────────────────────────────────────────────────────────────────────────
ratelimit:{api_key}:{unix_minute}     String  60s     Rate limit counter
session:{session_token}               Hash    24h     User session data
upload:{upload_id}:progress           Hash    7d      Parts received so far
upload:{upload_id}:lock               String  30s     Prevent concurrent completes
refresh:{user_id}                     String  7d      Refresh token
```

**Rate limiting algorithm (sliding window):**
```
On each request:
  key = "ratelimit:{api_key}:{current_minute}"
  count = INCR key
  if count == 1: EXPIRE key 60
  if count > 1000: return 429 Too Many Requests
```

---

### 3.6 Nginx (Local Reverse Proxy)

**Purpose:** Routes subdomains to correct services, buffers connections, handles large uploads.

```nginx
# /etc/nginx/sites-available/s3local

# Route api.yourdomain.com → Fastify API
server {
    listen 80;
    server_name api.yourdomain.com;

    client_max_body_size 0;           # no upload size limit
    proxy_request_buffering off;      # stream directly, don't buffer to disk
    proxy_read_timeout 600s;          # 10 min timeout for huge uploads
    proxy_send_timeout 600s;

    location / {
        proxy_pass         http://localhost:3000;
        proxy_http_version 1.1;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;
    }
}

# Route dash.yourdomain.com → Next.js Dashboard
server {
    listen 80;
    server_name dash.yourdomain.com;

    location / {
        proxy_pass         http://localhost:3001;
        proxy_http_version 1.1;
        proxy_set_header   Upgrade    $http_upgrade;
        proxy_set_header   Connection "upgrade";   # for Next.js HMR websocket
    }
}
```

---

### 3.7 Cloudflare Tunnel (Global Access)

**Purpose:** Expose your laptop to the internet — no static IP, no ISP port forwarding, Cloudflare handles TLS automatically.

**How it works internally:**
```
Your laptop runs cloudflared daemon
  → Opens persistent outbound connection to Cloudflare edge
    → Acts like a reverse tunnel (like ngrok but production-grade)
      → Cloudflare routes api.yourdomain.com traffic INTO this tunnel
        → cloudflared forwards to localhost:80 (Nginx)
          → No inbound ports needed. Your firewall stays closed.
```

**Full setup commands:**
```bash
# 1. Install cloudflared
curl -L https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-amd64.deb \
  -o cloudflared.deb
sudo dpkg -i cloudflared.deb

# 2. Login (opens browser, link your Cloudflare account)
cloudflared tunnel login

# 3. Create the tunnel (generates credentials file)
cloudflared tunnel create personal-s3
# Outputs tunnel UUID e.g. "abc123..."

# 4. Create config file
mkdir -p ~/.cloudflared
cat > ~/.cloudflared/config.yml << 'EOF'
tunnel: personal-s3
credentials-file: /home/YOUR_USER/.cloudflared/abc123....json

ingress:
  - hostname: api.yourdomain.com
    service: http://localhost:80
  - hostname: dash.yourdomain.com
    service: http://localhost:80
  - service: http_status:404    # catch-all for unknown hostnames
EOF

# 5. Create DNS records in Cloudflare (automatic)
cloudflared tunnel route dns personal-s3 api.yourdomain.com
cloudflared tunnel route dns personal-s3 dash.yourdomain.com

# 6. Test it manually first
cloudflared tunnel run personal-s3

# 7. Install as systemd service (survives reboots)
sudo cloudflared service install
sudo systemctl enable cloudflared
sudo systemctl start cloudflared

# Verify it's running
sudo systemctl status cloudflared
```

**Cloudflare dashboard settings to configure:**
- SSL/TLS mode: **Full** (Cloudflare ↔ origin encrypted)
- Always Use HTTPS: **On**
- HTTP/3 (QUIC): **On** (faster for large uploads)
- Under Tunnel → your tunnel: verify both hostnames show "Healthy"

---

## 4. Full Data Flow: End-to-End Upload

```
Step 1: Authentication
  Client → POST /auth/login {email, password}
  Server → validates, returns {token: "Bearer xyz..."}
  Client stores token for future requests

Step 2: Create bucket (if not exists)
  Client → PUT /my-videos
  Server → INSERT INTO buckets, mkdir /storage/buckets/{uuid}
  → 200 OK

Step 3: Initiate multipart upload (file > 5MB)
  Client → POST /my-videos/vacation.mp4?uploads
  Server → INSERT INTO multipart_uploads, generate upload_id "abc"
  → 200 OK {uploadId: "abc"}

Step 4: Upload parts (browser splits file into 5MB chunks)
  Client → PUT /my-videos/vacation.mp4?partNumber=1&uploadId=abc (5MB)
  Server → write /storage/buckets/{id}/tmp/abc/part-0001
         → UPDATE multipart_uploads SET parts = parts || [{1, etag1, size}]
  → 200 OK {ETag: "md5hash"}

  ... repeat for all parts in parallel (4 at a time) ...

Step 5: Complete upload
  Client → POST /my-videos/vacation.mp4?uploadId=abc
           {parts: [{partNumber:1,etag:"..."}, {partNumber:2,etag:"..."}, ...]}
  Server →
    1. Verify all parts present and ETags match
    2. cat /storage/.../tmp/abc/part-* > /storage/.../objects/{hash}/data
    3. Calculate final ETag
    4. DELETE tmp parts
    5. INSERT INTO objects (size, etag, path, ...)
    6. UPDATE users SET used_bytes = used_bytes + file_size  (atomic)
    7. UPDATE multipart_uploads SET status = 'complete'
    8. INSERT INTO audit_log
  → 200 OK {ETag: "final-etag-40parts"}

Step 6: Dashboard reflects new usage
  Dashboard polls GET /admin/stats every 30s
  Quota bar updates: [████████░░] 8.2 GB / 10 GB
```

---

## 5. What You Can Do With This System

| Task | How |
|---|---|
| Upload a file via curl | `curl -T file.zip https://api.yourdomain.com/mybucket/file.zip -H "Authorization: Bearer YOUR_KEY"` |
| Download a file | `curl https://api.yourdomain.com/mybucket/file.zip -o file.zip` |
| List bucket contents | `curl https://api.yourdomain.com/mybucket` |
| Give someone a 5GB quota | Dashboard → Users → Set Quota |
| Check who uploaded what | Dashboard → Audit Log |
| Use with AWS CLI | `aws --endpoint-url https://api.yourdomain.com s3 cp file.txt s3://bucket/` |
| Mount as drive (future) | `s3fs bucket /mnt/s3 -o url=https://api.yourdomain.com` |
| Access from phone | Open dash.yourdomain.com in browser |

---

## 6. Tech Stack Summary

| Layer | Technology | Why |
|---|---|---|
| API Server | Fastify (Node.js) | Fast, native streaming, low overhead |
| Dashboard | Next.js | Full-stack React, easy to deploy |
| Metadata DB | PostgreSQL | ACID transactions for quota accounting |
| Cache / State | Redis | Sub-ms rate limiting and multipart state |
| Storage | Linux ext4 filesystem | Simple, reliable, zero overhead |
| Reverse Proxy | Nginx | Large upload buffering, subdomain routing |
| Global Tunnel | Cloudflare cloudflared | Free, hides home IP, auto-TLS certs |
| Containers | Docker + Docker Compose | One command to start everything |

---

## 7. Build Order

| Phase | What to build | Output |
|---|---|---|
| Phase 1 | PostgreSQL schema + Docker Compose | DB running locally |
| Phase 2 | Fastify API: PUT / GET / DELETE objects | Can upload/download via curl |
| Phase 3 | Auth + quota enforcement | Multi-user with limits |
| Phase 4 | Cloudflare tunnel setup | Globally accessible |
| Phase 5 | Multipart chunked upload | Files > 5MB work |
| Phase 6 | Next.js dashboard | Visual UI |
| Phase 7 | Admin panel + audit log | Full management |
| Phase 8 | S3 CLI compatibility layer | AWS CLI / SDKs work |

---

---

# 8. AWS S3: Complete Deep Dive

---

## 8.1 What S3 Actually Is

Amazon S3 (Simple Storage Service) launched in 2006. It is an **object store**, not a filesystem.

The key distinction:
| Filesystem | Object Store (S3) |
|---|---|
| Files in directories | Objects in flat namespaces |
| Mutate files in-place | Replace entire objects only |
| POSIX operations (seek, truncate) | Read all or nothing |
| Fast random access | Optimized for streaming |
| Limited scale | Virtually unlimited |

**S3's data model:**
```
Account
  └── Bucket (globally unique name, e.g. "my-photos-2024")
        └── Object
              ├── Key   (the "path": "photos/jan/img.jpg")
              ├── Value (the actual bytes: the image file)
              ├── Metadata (content-type, custom headers, etc.)
              └── Version (if versioning enabled)
```

**Durability guarantee:** 99.999999999% (eleven nines) — S3 replicates every object across ≥3 Availability Zones within a region. Losing a file is extraordinarily rare.

**Scale:** S3 currently stores over 350 trillion objects. A single bucket can hold unlimited objects. Objects can be up to 5TB each.

---

## 8.2 Core Concepts

### Buckets

- Globally unique name across ALL AWS accounts (namespace is shared)
- Tied to a specific AWS region (data stays there by default)
- Naming rules: 3-63 chars, lowercase letters/numbers/hyphens, no underscores, no IP format
- Max 100 buckets per account by default (can request increase)

```bash
# Create bucket in us-east-1
aws s3api create-bucket --bucket my-unique-bucket-name --region us-east-1

# Create in other regions (requires LocationConstraint)
aws s3api create-bucket \
  --bucket my-bucket \
  --region eu-west-1 \
  --create-bucket-configuration LocationConstraint=eu-west-1
```

### Objects

- Key = the full "path" string including slashes (but slashes aren't real directories)
- Value = the bytes (0 bytes to 5TB)
- Metadata = HTTP headers + up to 2KB of custom key-value pairs
- ETag = usually MD5 of content (not guaranteed for multipart uploads)

```bash
# Upload
aws s3 cp localfile.txt s3://my-bucket/folder/file.txt

# The "folder" doesn't exist as a real entity — it's just part of the key name
# "folder/file.txt" is the entire key
```

### Keys and the "Folder" Illusion

```
S3 keys:
  "photos/2024/jan/img1.jpg"
  "photos/2024/jan/img2.jpg"
  "photos/2024/feb/img3.jpg"
  "videos/clip.mp4"

S3 console/AWS CLI groups by delimiter "/"
→ Shows "folders" photos/ and videos/
→ Inside photos/: shows 2024/
→ Inside 2024/: shows jan/ and feb/

None of these directories exist. They are a UI convenience.
The actual keys are the full strings above.
```

---

## 8.3 Storage Classes

S3 has 7 storage classes — you choose based on access frequency vs cost:

| Class | Use case | Retrieval | Min storage | Cost |
|---|---|---|---|---|
| **S3 Standard** | Frequent access | Milliseconds | None | Highest |
| **S3 Intelligent-Tiering** | Unknown/changing access | Milliseconds | None | Auto-optimizes |
| **S3 Standard-IA** | Infrequent access, rapid retrieval | Milliseconds | 30 days | Lower storage, retrieval fee |
| **S3 One Zone-IA** | Infrequent, single AZ, non-critical | Milliseconds | 30 days | Cheaper, less durable |
| **S3 Glacier Instant Retrieval** | Archive, quarterly access | Milliseconds | 90 days | Low storage |
| **S3 Glacier Flexible Retrieval** | Archive, hours acceptable | Minutes–12 hrs | 90 days | Very low storage |
| **S3 Glacier Deep Archive** | Compliance, once-a-year access | 12–48 hrs | 180 days | Cheapest |

**Set storage class on upload:**
```bash
aws s3 cp file.txt s3://bucket/file.txt --storage-class STANDARD_IA
aws s3 cp archive.zip s3://bucket/archive.zip --storage-class GLACIER
```

**Move objects to cheaper class via Lifecycle Rules** (covered in 8.4).

---

## 8.4 Object Lifecycle

Lifecycle rules automate moving or deleting objects over time.

**Example use case:** logs kept in Standard for 30 days, then move to Glacier, delete after 1 year.

```json
{
  "Rules": [
    {
      "ID": "archive-old-logs",
      "Status": "Enabled",
      "Filter": { "Prefix": "logs/" },
      "Transitions": [
        { "Days": 30,  "StorageClass": "STANDARD_IA" },
        { "Days": 90,  "StorageClass": "GLACIER" },
        { "Days": 365, "StorageClass": "DEEP_ARCHIVE" }
      ],
      "Expiration": { "Days": 730 }
    }
  ]
}
```

```bash
aws s3api put-bucket-lifecycle-configuration \
  --bucket my-bucket \
  --lifecycle-configuration file://lifecycle.json
```

**Other lifecycle actions:**
- `ExpiredObjectDeleteMarker`: clean up versioned delete markers
- `NoncurrentVersionExpiration`: delete old versions after N days
- `AbortIncompleteMultipartUpload`: clean up abandoned multipart uploads after N days (important — unfinished uploads cost money)

---

## 8.5 Versioning

When enabled, S3 keeps ALL versions of an object. Deleting adds a "delete marker" instead of actually deleting.

```bash
# Enable versioning
aws s3api put-bucket-versioning \
  --bucket my-bucket \
  --versioning-configuration Status=Enabled

# Upload same key twice
aws s3 cp file.txt s3://my-bucket/file.txt   # version ID: "aaa"
aws s3 cp file.txt s3://my-bucket/file.txt   # version ID: "bbb"

# List all versions
aws s3api list-object-versions --bucket my-bucket --prefix file.txt

# Get specific version
aws s3api get-object \
  --bucket my-bucket \
  --key file.txt \
  --version-id aaa \
  output.txt

# "Delete" adds a delete marker (object still exists!)
aws s3 rm s3://my-bucket/file.txt

# Permanently delete a specific version
aws s3api delete-object \
  --bucket my-bucket \
  --key file.txt \
  --version-id aaa
```

**Versioning states:**
- `Unversioned` (default) — no versions, overwrite destroys old data
- `Enabled` — all writes create new versions
- `Suspended` — stops new versions but keeps existing ones

---

## 8.6 Access Control

S3 has multiple overlapping access control mechanisms — this is the most complex part.

### IAM Policies (identity-based)
Attached to users/roles. Controls what that identity can do to any S3 resource.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject", "s3:PutObject"],
      "Resource": "arn:aws:s3:::my-bucket/*"
    },
    {
      "Effect": "Allow",
      "Action": "s3:ListBucket",
      "Resource": "arn:aws:s3:::my-bucket"
    }
  ]
}
```

### Bucket Policies (resource-based)
Attached to the bucket. Can grant access to other AWS accounts or public.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": "*",
      "Action": "s3:GetObject",
      "Resource": "arn:aws:s3:::my-public-bucket/*"
    }
  ]
}
```

```bash
aws s3api put-bucket-policy --bucket my-bucket --policy file://policy.json
```

### Block Public Access (safety override)
4 flags that override all other settings to prevent accidental public exposure.

```bash
aws s3api put-public-access-block \
  --bucket my-bucket \
  --public-access-block-configuration \
    "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true"
```

### ACLs (legacy, mostly deprecated)
Object-level and bucket-level ACLs. AWS now recommends disabling ACLs and using bucket policies instead.

```bash
# Make single object public (legacy approach)
aws s3api put-object-acl --bucket my-bucket --key file.txt --acl public-read
```

### Presigned URLs (covered in 8.9)
Time-limited signed URLs that grant temporary access without AWS credentials.

---

## 8.7 S3 API: Complete Reference

S3 exposes an HTTP REST API. All AWS SDK calls and CLI commands translate to these.

### Bucket Operations

| API | HTTP | Description |
|---|---|---|
| `CreateBucket` | `PUT /` | Create a new bucket |
| `DeleteBucket` | `DELETE /` | Delete empty bucket |
| `HeadBucket` | `HEAD /` | Check if bucket exists and you have access |
| `ListBuckets` | `GET /` | List all buckets in account |
| `GetBucketLocation` | `GET /?location` | Get bucket region |
| `PutBucketVersioning` | `PUT /?versioning` | Enable/suspend versioning |
| `GetBucketVersioning` | `GET /?versioning` | Get versioning state |
| `PutBucketLifecycleConfiguration` | `PUT /?lifecycle` | Set lifecycle rules |
| `PutBucketPolicy` | `PUT /?policy` | Set bucket policy |
| `GetBucketPolicy` | `GET /?policy` | Get bucket policy |
| `PutBucketCors` | `PUT /?cors` | Set CORS rules |
| `PutBucketEncryption` | `PUT /?encryption` | Set default encryption |
| `PutBucketNotificationConfiguration` | `PUT /?notification` | Set event notifications |
| `PutBucketReplication` | `PUT /?replication` | Set cross-region replication |
| `PutBucketWebsite` | `PUT /?website` | Enable static hosting |
| `PutBucketTagging` | `PUT /?tagging` | Tag a bucket |
| `GetBucketTagging` | `GET /?tagging` | Get bucket tags |
| `PutBucketLogging` | `PUT /?logging` | Enable server access logging |
| `GetBucketLogging` | `GET /?logging` | Get logging config |
| `PutObjectLockConfiguration` | `PUT /?object-lock` | Enable object lock |

### Object Operations

| API | HTTP | Description |
|---|---|---|
| `PutObject` | `PUT /:key` | Upload object (≤5GB single request) |
| `GetObject` | `GET /:key` | Download object |
| `DeleteObject` | `DELETE /:key` | Delete object |
| `HeadObject` | `HEAD /:key` | Get metadata only (no body) |
| `CopyObject` | `PUT /:key` + `x-amz-copy-source` header | Server-side copy |
| `ListObjectsV2` | `GET /?list-type=2` | List objects (current recommended) |
| `ListObjectVersions` | `GET /?versions` | List all object versions |
| `PutObjectTagging` | `PUT /:key?tagging` | Tag an object |
| `GetObjectTagging` | `GET /:key?tagging` | Get object tags |
| `RestoreObject` | `POST /:key?restore` | Restore from Glacier |
| `SelectObjectContent` | `POST /:key?select` | Run SQL query on CSV/JSON/Parquet |
| `WriteGetObjectResponse` | `POST /` | For Object Lambda transformations |
| `PutObjectRetention` | `PUT /:key?retention` | Set object lock retention |
| `PutObjectLegalHold` | `PUT /:key?legal-hold` | Set legal hold |
| `GetObjectLockConfiguration` | `GET /?object-lock` | Get lock config |

### Multipart Upload Operations

| API | HTTP | Description |
|---|---|---|
| `CreateMultipartUpload` | `POST /:key?uploads` | Initiate upload, get uploadId |
| `UploadPart` | `PUT /:key?partNumber=N&uploadId=X` | Upload one part |
| `UploadPartCopy` | `PUT /:key?partNumber=N&uploadId=X` + copy header | Copy part server-side |
| `CompleteMultipartUpload` | `POST /:key?uploadId=X` | Assemble parts into final object |
| `AbortMultipartUpload` | `DELETE /:key?uploadId=X` | Cancel and delete parts |
| `ListMultipartUploads` | `GET /?uploads` | List in-progress uploads |
| `ListParts` | `GET /:key?uploadId=X` | List uploaded parts |

---

## 8.8 Multipart Upload (Deep Dive)

Required for objects > 5GB. Recommended for objects > 100MB. Minimum part size is 5MB (except last part).

```
Max parts per upload: 10,000
Min part size:        5 MB (except last)
Max part size:        5 GB
Max object size:      5 TB
```

**Full flow in Python (boto3):**
```python
import boto3
import os

s3 = boto3.client('s3')
bucket = 'my-bucket'
key = 'large-file.zip'
filepath = '/tmp/large-file.zip'
chunk_size = 5 * 1024 * 1024  # 5MB

# 1. Initiate
response = s3.create_multipart_upload(Bucket=bucket, Key=key)
upload_id = response['UploadId']

parts = []
file_size = os.path.getsize(filepath)

with open(filepath, 'rb') as f:
    part_number = 1
    while True:
        data = f.read(chunk_size)
        if not data:
            break

        # 2. Upload each part
        response = s3.upload_part(
            Bucket=bucket,
            Key=key,
            PartNumber=part_number,
            UploadId=upload_id,
            Body=data
        )
        parts.append({'PartNumber': part_number, 'ETag': response['ETag']})
        part_number += 1

# 3. Complete
s3.complete_multipart_upload(
    Bucket=bucket,
    Key=key,
    UploadId=upload_id,
    MultipartUpload={'Parts': parts}
)
```

**boto3 shortcut (handles multipart automatically):**
```python
# TransferConfig controls when multipart kicks in
from boto3.s3.transfer import TransferConfig

config = TransferConfig(
    multipart_threshold=8 * 1024 * 1024,   # 8MB: use multipart above this
    max_concurrency=10,                      # 10 parallel part uploads
    multipart_chunksize=8 * 1024 * 1024,   # 8MB chunks
    use_threads=True
)

s3.upload_file('large.zip', 'my-bucket', 'large.zip', Config=config)
```

---

## 8.9 Presigned URLs

Grant time-limited access to private objects without sharing credentials.

**Use cases:**
- Let a user download a private file for 1 hour
- Let a frontend directly upload to S3 without going through your server
- Share a file with a non-AWS user

```python
import boto3

s3 = boto3.client('s3', region_name='us-east-1')

# Presigned GET URL (download for 1 hour)
url = s3.generate_presigned_url(
    'get_object',
    Params={'Bucket': 'my-bucket', 'Key': 'private/file.pdf'},
    ExpiresIn=3600  # seconds
)
# Returns: https://my-bucket.s3.amazonaws.com/private/file.pdf?X-Amz-Signature=...

# Presigned PUT URL (let client upload directly)
url = s3.generate_presigned_url(
    'put_object',
    Params={
        'Bucket': 'my-bucket',
        'Key': 'uploads/user123/photo.jpg',
        'ContentType': 'image/jpeg'
    },
    ExpiresIn=300  # 5 minutes
)

# Client uploads directly to S3 using the URL:
# curl -X PUT -T photo.jpg -H "Content-Type: image/jpeg" "URL"
```

**Presigned POST (more control than PUT — enforce size limits, content type):**
```python
response = s3.generate_presigned_post(
    Bucket='my-bucket',
    Key='uploads/${filename}',
    Fields={'Content-Type': 'image/jpeg'},
    Conditions=[
        ['content-length-range', 1, 5 * 1024 * 1024],  # 1 byte to 5MB
        ['eq', '$Content-Type', 'image/jpeg']
    ],
    ExpiresIn=300
)
# Returns: {url: "https://...", fields: {key, policy, signature, ...}}
# Client submits multipart/form-data form to this URL
```

---

## 8.10 S3 Events & Notifications

S3 can trigger events when objects are created, deleted, or restored.

**Supported targets:**
- Amazon SNS (email/SMS notifications)
- Amazon SQS (message queue for processing)
- AWS Lambda (serverless function)
- Amazon EventBridge (routing to many targets)

```json
{
  "LambdaFunctionConfigurations": [
    {
      "LambdaFunctionArn": "arn:aws:lambda:us-east-1:123456789:function:ProcessImage",
      "Events": ["s3:ObjectCreated:*"],
      "Filter": {
        "Key": {
          "FilterRules": [
            {"Name": "prefix", "Value": "uploads/"},
            {"Name": "suffix", "Value": ".jpg"}
          ]
        }
      }
    }
  ]
}
```

```bash
aws s3api put-bucket-notification-configuration \
  --bucket my-bucket \
  --notification-configuration file://notification.json
```

**Event types available:**
```
s3:ObjectCreated:*          → any creation event
s3:ObjectCreated:Put        → PutObject
s3:ObjectCreated:Post       → PostObject (presigned POST)
s3:ObjectCreated:Copy       → CopyObject
s3:ObjectCreated:CompleteMultipartUpload
s3:ObjectRemoved:*          → any deletion
s3:ObjectRemoved:Delete
s3:ObjectRemoved:DeleteMarkerCreated
s3:ObjectRestore:*          → Glacier restore
s3:Replication:*            → replication events
s3:LifecycleTransition      → tier change
```

---

## 8.11 Static Website Hosting

S3 can serve a full static website directly (no server needed).

```bash
# Enable static hosting
aws s3api put-bucket-website \
  --bucket my-website.com \
  --website-configuration '{
    "IndexDocument": {"Suffix": "index.html"},
    "ErrorDocument": {"Key": "error.html"}
  }'

# Upload site files
aws s3 sync ./dist s3://my-website.com/

# Website endpoint (not the same as the S3 API endpoint)
# http://my-website.com.s3-website-us-east-1.amazonaws.com

# Custom domain: set up CloudFront in front + Route53 for DNS
```

**Limitations:**
- HTTP only (HTTPS requires CloudFront)
- No server-side logic (static files only)
- Bucket name must match domain name

---

## 8.12 Replication

Copy objects to another bucket automatically as they're uploaded.

**Cross-Region Replication (CRR):** bucket in us-east-1 → bucket in eu-west-1
**Same-Region Replication (SRR):** both buckets same region

**Requirements:** versioning must be enabled on both source and destination.

```json
{
  "Role": "arn:aws:iam::123456789:role/s3-replication-role",
  "Rules": [
    {
      "Status": "Enabled",
      "Prefix": "important/",
      "Destination": {
        "Bucket": "arn:aws:s3:::my-replica-bucket",
        "StorageClass": "STANDARD_IA",
        "ReplicationTime": {
          "Status": "Enabled",
          "Time": {"Minutes": 15}
        }
      }
    }
  ]
}
```

```bash
aws s3api put-bucket-replication \
  --bucket source-bucket \
  --replication-configuration file://replication.json
```

**Note:** Replication is not retroactive — only new objects are replicated after enabling.

---

## 8.13 S3 Select & Glacier Select

Query data inside S3 objects without downloading the entire file.

Supported formats: **CSV, JSON, Apache Parquet**

```python
import boto3

s3 = boto3.client('s3')

response = s3.select_object_content(
    Bucket='my-bucket',
    Key='data/sales-2024.csv',
    ExpressionType='SQL',
    Expression="SELECT * FROM s3object WHERE revenue > 10000",
    InputSerialization={
        'CSV': {'FileHeaderInfo': 'USE', 'RecordDelimiter': '\n', 'FieldDelimiter': ','}
    },
    OutputSerialization={'JSON': {'RecordDelimiter': '\n'}}
)

for event in response['Payload']:
    if 'Records' in event:
        print(event['Records']['Payload'].decode('utf-8'))
```

**Why this matters:** A 10GB CSV → SQL filter → download only 50MB result. You pay for 50MB data transfer, not 10GB.

---

## 8.14 Transfer Acceleration

Routes uploads through Cloudflare's (AWS's) global edge network for faster long-distance uploads.

```bash
# Enable on bucket
aws s3api put-bucket-accelerate-configuration \
  --bucket my-bucket \
  --accelerate-configuration Status=Enabled

# Use accelerated endpoint
aws s3 cp large-file.zip s3://my-bucket/ \
  --endpoint-url https://my-bucket.s3-accelerate.amazonaws.com
```

**When it helps:** Users uploading from far regions (e.g., user in Asia to bucket in us-east-1). Data enters the nearest AWS edge POP and travels the backbone instead of the public internet.

**Cost:** Additional per-GB transfer fee on top of standard transfer costs.

---

## 8.15 Inventory, Analytics, Metrics

### S3 Inventory
Generate daily or weekly CSV/ORC/Parquet report of all objects in a bucket — useful for auditing, billing analysis.

```bash
aws s3api put-bucket-inventory-configuration \
  --bucket source-bucket \
  --id my-inventory \
  --inventory-configuration '{
    "Id": "my-inventory",
    "IsEnabled": true,
    "Destination": {
      "S3BucketDestination": {
        "Bucket": "arn:aws:s3:::my-reports-bucket",
        "Format": "CSV"
      }
    },
    "Schedule": {"Frequency": "Daily"},
    "IncludedObjectVersions": "All",
    "OptionalFields": ["Size", "LastModifiedDate", "StorageClass", "ETag"]
  }'
```

### S3 Analytics
Analyze access patterns to decide when to move objects to Standard-IA.

### S3 Storage Lens
Organization-wide visibility across all buckets — usage, activity, cost optimization recommendations.

### CloudWatch Metrics
Request metrics (with extra cost), replication metrics, storage metrics.

```bash
# Enable request metrics on bucket
aws s3api put-bucket-metrics-configuration \
  --bucket my-bucket \
  --id all-objects \
  --metrics-configuration '{"Id": "all-objects"}'
```

---

## 8.16 Object Lock & Compliance

Prevent object deletion for a set time — WORM (Write Once Read Many) storage for compliance.

**Two modes:**
- **Governance:** Users with special permissions can override
- **Compliance:** Nobody (not even root) can delete during retention period

```bash
# Enable object lock (bucket must be new — cannot enable on existing bucket)
aws s3api create-bucket --bucket compliance-bucket --region us-east-1
aws s3api put-object-lock-configuration \
  --bucket compliance-bucket \
  --object-lock-configuration '{
    "ObjectLockEnabled": "Enabled",
    "Rule": {
      "DefaultRetention": {
        "Mode": "COMPLIANCE",
        "Years": 7
      }
    }
  }'
```

**Legal Hold** — indefinite lock without a retention date:
```bash
aws s3api put-object-legal-hold \
  --bucket my-bucket \
  --key evidence.pdf \
  --legal-hold Status=ON
```

---

## 8.17 Intelligent Tiering

S3 monitors access patterns and automatically moves objects between tiers.

**Tiers within Intelligent-Tiering:**
```
Frequent Access tier         → accessed recently
Infrequent Access tier       → not accessed for 30 days (lower cost)
Archive Instant Access tier  → not accessed for 90 days  (even lower)
Archive Access tier          → not accessed for 90-180 days (Glacier speed)
Deep Archive Access tier     → not accessed for 180+ days (Deep Archive speed)
```

**When to use:** When you genuinely don't know if data will be accessed. The small monitoring fee per object is offset by automatic savings on rarely-accessed data.

```bash
aws s3 cp file.txt s3://my-bucket/file.txt --storage-class INTELLIGENT_TIERING
```

---

## 8.18 Using S3 with AWS CLI

```bash
# Configuration
aws configure                          # set access key, secret key, region, output format

# High-level commands (s3 subcommand — easy to use)
aws s3 ls                              # list all buckets
aws s3 ls s3://my-bucket/             # list bucket contents
aws s3 ls s3://my-bucket/photos/ --recursive  # list recursively

aws s3 cp file.txt s3://my-bucket/    # upload
aws s3 cp s3://my-bucket/file.txt .   # download
aws s3 cp s3://bucket1/key s3://bucket2/key  # copy between buckets

aws s3 mv file.txt s3://my-bucket/    # upload and delete local
aws s3 mv s3://my-bucket/old.txt s3://my-bucket/new.txt  # rename (copy+delete)

aws s3 rm s3://my-bucket/file.txt     # delete object
aws s3 rm s3://my-bucket/ --recursive # delete all objects

aws s3 sync ./local-dir s3://my-bucket/prefix/     # sync local → S3
aws s3 sync s3://my-bucket/prefix/ ./local-dir     # sync S3 → local
aws s3 sync s3://source-bucket/ s3://dest-bucket/  # sync between buckets
aws s3 sync ./dir s3://bucket/ --exclude "*.log" --include "*.jpg"

aws s3 website s3://my-bucket/ --index-document index.html

# Low-level commands (s3api — full API access)
aws s3api list-objects-v2 --bucket my-bucket --prefix "photos/" --max-keys 100
aws s3api head-object --bucket my-bucket --key file.txt
aws s3api get-object --bucket my-bucket --key file.txt output.txt
aws s3api put-object --bucket my-bucket --key file.txt --body file.txt \
  --content-type text/plain --metadata '{"author":"Alice"}'
aws s3api delete-object --bucket my-bucket --key file.txt
aws s3api copy-object \
  --copy-source my-bucket/source.txt \
  --bucket my-bucket --key dest.txt

# Presigned URL
aws s3 presign s3://my-bucket/private.pdf --expires-in 3600

# Sync with delete (makes destination exactly match source)
aws s3 sync ./dir s3://bucket/ --delete
```

---

## 8.19 Using S3 with SDKs

### Python (boto3)

```python
import boto3
from botocore.exceptions import ClientError

s3 = boto3.client('s3',
    region_name='us-east-1',
    aws_access_key_id='YOUR_KEY',        # or use env vars / IAM role
    aws_secret_access_key='YOUR_SECRET'
)

# Upload
s3.upload_file('local.txt', 'my-bucket', 'remote.txt')

# Upload with metadata
s3.put_object(
    Bucket='my-bucket',
    Key='file.txt',
    Body=b'hello world',
    ContentType='text/plain',
    Metadata={'author': 'alice', 'project': 'demo'}
)

# Download
s3.download_file('my-bucket', 'remote.txt', 'local.txt')

# Stream download (for large files)
response = s3.get_object(Bucket='my-bucket', Key='large.zip')
with open('large.zip', 'wb') as f:
    for chunk in response['Body'].iter_chunks(chunk_size=1024*1024):
        f.write(chunk)

# List objects
paginator = s3.get_paginator('list_objects_v2')
for page in paginator.paginate(Bucket='my-bucket', Prefix='photos/'):
    for obj in page.get('Contents', []):
        print(obj['Key'], obj['Size'])

# Check if object exists
try:
    s3.head_object(Bucket='my-bucket', Key='file.txt')
    print("exists")
except ClientError as e:
    if e.response['Error']['Code'] == '404':
        print("not found")

# Delete
s3.delete_object(Bucket='my-bucket', Key='file.txt')

# Generate presigned URL
url = s3.generate_presigned_url(
    'get_object',
    Params={'Bucket': 'my-bucket', 'Key': 'private.pdf'},
    ExpiresIn=3600
)
```

### JavaScript / Node.js (AWS SDK v3)

```javascript
import { S3Client, PutObjectCommand, GetObjectCommand,
         ListObjectsV2Command, DeleteObjectCommand } from '@aws-sdk/client-s3'
import { getSignedUrl } from '@aws-sdk/s3-request-presigner'
import { Upload } from '@aws-sdk/lib-storage'
import { createReadStream, createWriteStream } from 'fs'

const s3 = new S3Client({
  region: 'us-east-1',
  credentials: {
    accessKeyId: process.env.AWS_ACCESS_KEY_ID,
    secretAccessKey: process.env.AWS_SECRET_ACCESS_KEY
  }
})

// Upload small file
await s3.send(new PutObjectCommand({
  Bucket: 'my-bucket',
  Key: 'file.txt',
  Body: Buffer.from('hello'),
  ContentType: 'text/plain'
}))

// Upload large file with multipart (automatic)
const upload = new Upload({
  client: s3,
  params: {
    Bucket: 'my-bucket',
    Key: 'large.zip',
    Body: createReadStream('/tmp/large.zip')
  },
  queueSize: 4,           // 4 parallel part uploads
  partSize: 5 * 1024 * 1024,  // 5MB per part
})
upload.on('httpUploadProgress', (progress) => {
  console.log(`${progress.loaded} / ${progress.total}`)
})
await upload.done()

// Download and stream to file
const response = await s3.send(new GetObjectCommand({
  Bucket: 'my-bucket', Key: 'file.txt'
}))
response.Body.pipe(createWriteStream('output.txt'))

// List objects
const result = await s3.send(new ListObjectsV2Command({
  Bucket: 'my-bucket',
  Prefix: 'photos/',
  MaxKeys: 100
}))
result.Contents?.forEach(obj => console.log(obj.Key, obj.Size))

// Presigned URL
const url = await getSignedUrl(s3, new GetObjectCommand({
  Bucket: 'my-bucket', Key: 'private.pdf'
}), { expiresIn: 3600 })

// Use with custom endpoint (for PersonalS3!)
const personalS3 = new S3Client({
  region: 'us-east-1',
  endpoint: 'https://api.yourdomain.com',
  forcePathStyle: true,      // required for non-AWS endpoints
  credentials: {
    accessKeyId: 'your-api-key',
    secretAccessKey: 'your-api-secret'
  }
})
```

### Go (aws-sdk-go-v2)

```go
import (
    "context"
    "github.com/aws/aws-sdk-go-v2/config"
    "github.com/aws/aws-sdk-go-v2/service/s3"
)

cfg, _ := config.LoadDefaultConfig(context.TODO(), config.WithRegion("us-east-1"))
client := s3.NewFromConfig(cfg)

// Upload
client.PutObject(context.TODO(), &s3.PutObjectInput{
    Bucket: aws.String("my-bucket"),
    Key:    aws.String("file.txt"),
    Body:   strings.NewReader("hello"),
})

// Download
result, _ := client.GetObject(context.TODO(), &s3.GetObjectInput{
    Bucket: aws.String("my-bucket"),
    Key:    aws.String("file.txt"),
})
defer result.Body.Close()
io.Copy(os.Stdout, result.Body)
```

---

---

# 9. Coverage Map: What Plan-1 Covers vs What S3 Has

## ✅ COVERED IN PLAN-1

These features are fully planned and will be built:

| S3 Feature | Plan-1 Equivalent | Notes |
|---|---|---|
| Buckets | ✅ Buckets (PostgreSQL `buckets` table) | Create, delete, list |
| Object upload (single) | ✅ `PUT /:bucket/:key` | Files ≤ 5GB |
| Object download | ✅ `GET /:bucket/:key` | Full streaming |
| Object delete | ✅ `DELETE /:bucket/:key` | Hard delete |
| Object metadata | ✅ `meta.json` + DB columns | Content-type, ETag, custom headers |
| List objects | ✅ `GET /:bucket` | With prefix/delimiter/marker |
| Multipart upload | ✅ Full flow | 5MB chunks, parallel parts |
| ETag (S3-compatible) | ✅ MD5 single / MD5-of-MD5s multipart | |
| Storage quotas per user | ✅ `quota_bytes` + atomic SQL | Better than S3 (S3 has no per-user quotas) |
| API key auth | ✅ Bearer token auth | Similar to AWS access keys |
| Rate limiting | ✅ Redis-backed per-key | S3 doesn't expose this to users |
| Audit logging | ✅ `audit_log` table | Who did what and when |
| Dashboard | ✅ Next.js UI | S3 has AWS Console |
| Global access | ✅ Cloudflare Tunnel | S3 uses AWS global infra |
| HTTPS / TLS | ✅ Cloudflare handles it | |
| AWS CLI compatibility | ✅ `--endpoint-url` flag | Phase 8 of build |

---

## ⚠️ PARTIALLY COVERED (simplified version)

| S3 Feature | What Plan-1 Has | What's Missing |
|---|---|---|
| Access control | API key per user, ownership check | No IAM-style policies, no resource-based policies, no cross-account |
| Object copy | Not planned yet | S3 `CopyObject` is server-side (no re-upload needed) |
| Presigned URLs | Not planned | Time-limited signed download/upload URLs |
| CORS | Not planned | Browser-based direct uploads would need it |
| Object tagging | `metadata` JSONB field | Not a dedicated tagging API |

---

## ❌ NOT COVERED (out of scope for Plan-1)

These are S3 features not planned. They can be added in future phases.

| S3 Feature | Complexity | Future Phase? |
|---|---|---|
| **Versioning** | Medium | Phase 9 — version_id column exists in schema |
| **Lifecycle rules** | Medium | Phase 10 — move to archive or auto-delete after N days |
| **Presigned URLs** | Low | Phase 9 — HMAC-signed time-limited URLs |
| **Static website hosting** | Low | Serve bucket contents as a website |
| **S3 Events / Notifications** | High | Trigger webhooks on upload/delete |
| **Server-side encryption (SSE)** | Medium | Encrypt data at rest with AES-256 |
| **Object Lock / WORM** | Medium | Compliance: prevent deletion |
| **Replication** | High | Copy to another machine/location |
| **S3 Select** | High | SQL queries on CSV/JSON without full download |
| **Transfer Acceleration** | N/A | Cloudflare already handles edge acceleration |
| **Intelligent Tiering** | High | Auto-move old files to "cold" storage |
| **Storage Classes** | Medium | Hot/cold tiers (SSD vs HDD partition) |
| **Inventory reports** | Low | CSV export of all objects |
| **Server access logging** | Low | Request logs to a separate bucket |
| **Object Lambda** | Very High | Transform objects on read |
| **Multi-region** | Very High | Distribute across multiple machines |
| **Batch Operations** | High | Bulk operations on millions of objects |

---

## FEATURE GAP SUMMARY

```
                    AWS S3          Plan-1 (Phase 1-8)
                 ──────────────   ───────────────────────
Core Storage:    ██████████ 100%  ████████░░ 80%
Auth/Access:     ██████████ 100%  █████░░░░░ 50%
Multipart:       ██████████ 100%  ██████████ 100%
Versioning:      ██████████ 100%  ░░░░░░░░░░  0%  (schema ready)
Lifecycle:       ██████████ 100%  ░░░░░░░░░░  0%
Events:          ██████████ 100%  ░░░░░░░░░░  0%
Encryption:      ██████████ 100%  ░░░░░░░░░░  0%
Analytics:       ██████████ 100%  ██░░░░░░░░ 20%  (audit log only)
Compliance:      ██████████ 100%  ░░░░░░░░░░  0%
Presigned URLs:  ██████████ 100%  ░░░░░░░░░░  0%  (Phase 9)
Dashboard:       ██████████ 100%  ████████░░ 80%
Quotas:          ░░░░░░░░░░  0%   ██████████ 100% ← Plan-1 wins here
```

**Plan-1 covers the most important 80% of daily S3 use cases.**
The remaining 20% (versioning, lifecycle, encryption, events) can be added incrementally.

---

*End of Plan-1*
