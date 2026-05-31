# PersonalS3 — Documentation

A self-hosted, S3-compatible object storage system for one person (or a
small team) with built-in media transcoding, a web dashboard, a native CLI,
near-real-time integrity verification, and optional global access via
Cloudflare Tunnel. Runs entirely in Docker on your own hardware.

---

## Start here

### First-time setup

1. **[getting-started.md](./getting-started.md)** — install, first upload, ~10 min
2. **[storage-management.md](./storage-management.md)** — disk capacity, quotas, adding more space
3. **[cloudflare-tunnel.md](./cloudflare-tunnel.md)** — global access without port-forwarding

### Day-to-day usage

4. **[cli.md](./cli.md)** — the `ps3` command-line client (recommended for scripts)
5. **[clients/](./clients/)** — other ways in:
   - [Browser dashboard](./clients/dashboard.md)
   - [curl / shell](./clients/curl.md)
   - [AWS CLI](./clients/aws-cli.md)
   - [boto3 (Python)](./clients/boto3.md)
   - [rclone (sync)](./clients/rclone.md)
6. **[api-reference.md](./api-reference.md)** — every HTTP endpoint
7. **[multipart.md](./multipart.md)** — multipart upload protocol + S3 differences + per-client recipes
8. **[user-management.md](./user-management.md)** — accounts, API keys, quotas

### Operations & reliability

9. **[production-tuning.md](./production-tuning.md)** — pre-launch checklist (backup, knobs, monitoring)
10. **[operations.md](./operations.md)** — monitoring, backups, log rotation
11. **[troubleshooting.md](./troubleshooting.md)** — when things break

### Internals (read if curious)

12. **[architecture/](./architecture/)** — component diagrams + data flows
13. **[come-up-designs/](./come-up-designs/)** — design notebooks: how we arrived
    at the current implementation, with the rejected alternatives. Starting
    with [`cleaner.md`](./come-up-designs/cleaner.md) — V1 (pure DB) → V2
    (bloom) → V3 (fixed shards) → V4 (adaptive Merkle trie) with scaling
    matrices and queries.

### Development

14. **[development.md](./development.md)** — code layout, adding features, testing

---

## Components at a glance

| Container | Role |
|---|---|
| `api`      | Go HTTP server — auth, buckets, objects, multipart, presigned URLs, search, admin |
| `worker`   | Python — FFmpeg + Pillow for video/audio/image transcoding with hardware accel |
| `cleaner`  | Go — garbage collection + integrity verification + fsnotify watcher (V4 trie) |
| `dashboard`| Next.js — file-manager UI |
| `nginx`    | Reverse proxy, static HLS serving |
| `postgres` | Metadata + manifest |
| `valkey`   | Rate limiting, cancel pub/sub |
| `cloudflared-quick` (opt-in profile) | Cloudflare Tunnel |

## TL;DR

```bash
# First time — copy example .env then generate secrets
cp .env.example .env   # if it exists
./scripts/init-secrets.sh

# Bring everything up
docker compose up -d

# Web UI
xdg-open http://localhost:8080
# Default admin login from .env (ADMIN_EMAIL / ADMIN_PASSWORD)

# Or use the CLI (after `cd cli && go install ./cmd/ps3`)
ps3 login --server http://localhost:8080
ps3 bucket list
```

## What was added after the initial build

The base docs in `clients/`, `architecture/`, and so on describe v1. The
following features ship today and may not be reflected in every other doc
yet — see the linked sections of `api-reference.md` for canonical behavior:

| Feature | Where |
|---|---|
| **Pre-signed share URLs (GET)** | `POST /:bucket/:key?presign` |
| **Pre-signed PUT URLs** (direct uploads) | same, with `{"method":"PUT"}` |
| **Public buckets** | `PATCH` `isPublic`, served at `/public/{bucket}/...` |
| **Object versioning** | `PATCH` `versioning`, `?versions` / `?restore` / `?versionId=` |
| **Soft delete / trash** | default for `DELETE`; `?purge` to skip |
| **Folder navigation** | `?prefix=` + `?delimiter=/` returns `commonPrefixes` |
| **Cross-bucket search** | `GET /search?q=&bucket=&type=&ext=&minSize=` |
| **Bucket archival** | `PATCH` `archived` — cleaner skips its shards |
| **Image on-the-fly resize** | `GET /:bucket/key?w=600&fit=cover&q=85&fmt=webp` |
| **Dynamic transcode ladder** | API probes source height; enqueues only applicable rungs |
| **V4 cleaner** | adaptive Merkle trie + fsnotify + bucket-root sweep; near-real-time orphan detection |
| **Transcode quota reservation** | pre-flight `+estimate` + publish-point delta settle — closes TOCTOU on concurrent uploads ([storage-management.md](./storage-management.md)) |
| **Per-bucket storage breakdown** | `GET /auth/storage` powers dashboard stacked chart with bucket / trash / versions / reserved slices |
| **Multipart upload protocol** | full reference + per-client recipes + S3 differences ([multipart.md](./multipart.md)) |
| **Quota error details** | `507 QUOTA_EXCEEDED` responses now include `{requestedBytes, availableBytes, deficitBytes, ...}` |
| **ps3 CLI** | [cli.md](./cli.md) |
| **Backup + restore + production tuning** | [`scripts/backup.sh`](../scripts/backup.sh), [production-tuning.md](./production-tuning.md) |

## Caveats

- **Not internet-exposed by default.** Local-network use is fine; before
  putting this on a public domain enable HTTPS (via Cloudflare Tunnel is
  easiest), add 2FA, and run a security review.
- **Single-host.** Postgres + storage volume live on one machine. The
  backup script covers data, but recovery means "restore on a new host."
- **Quotas not auto-reconciled.** Run
  [`scripts/quota-reconcile.sql`](../scripts/quota-reconcile.sql) weekly
  (it's in the cron snippet in [production-tuning.md](./production-tuning.md)).
  Drift is rare under normal use — the reservation column closes the
  multi-upload race, and worker FFmpeg failures now refund — but the
  script is the canonical "make it true again" safety net.

- **CLI doesn't multipart yet.** Single-`PUT` per file. Use the dashboard
  or AWS CLI for multi-GB uploads where resume matters. See
  [multipart.md](./multipart.md#ps3-cli) for the workaround table.
