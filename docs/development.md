# Development

For when you want to change the code.

## Project layout

```
S3orSimilar/
├── docker-compose.yml         orchestrates all services
├── .env / .env.example        per-deployment secrets and config
├── README.md                  user-facing entry point
├── plan-1.md                  the original spec (historical)
│
├── api/                       Go API server
│   ├── Dockerfile             multi-stage build, no local Go needed
│   ├── go.mod
│   ├── cmd/server/main.go     entry: wire deps, register routes
│   └── internal/
│       ├── config/                env loader
│       ├── db/                    pgx pool
│       ├── cache/                 valkey/redis client
│       ├── storage/               filesystem layout, statfs
│       ├── sysconfig/             system_config helpers
│       ├── auth/                  password/apikey/jwt/sigv4
│       ├── middleware/            chi-compatible middleware
│       ├── handlers/              HTTP handlers (one file per resource)
│       └── httpx/                 response helpers
│
├── worker/                    Python transcoding worker
│   ├── Dockerfile             debian + ffmpeg + libaom + Pillow
│   ├── pyproject.toml
│   └── worker/
│       ├── main.py            poll loop
│       ├── db.py              SELECT...FOR UPDATE SKIP LOCKED
│       ├── transcoder.py      dispatch by file type
│       ├── video.py           HLS multi-bitrate
│       ├── audio.py           HLS + MP3 + OGG
│       └── image.py           WebP/AVIF/thumbs via Pillow + ffmpeg
│
├── dashboard/                 Next.js 15 + Tailwind
│   ├── Dockerfile             standalone output
│   ├── package.json
│   ├── app/                   App Router pages
│   ├── components/            React UI
│   └── lib/                   API client, multipart, formatting
│
├── nginx/                     reverse proxy + static HLS
│   └── nginx.conf
│
├── cloudflared/               Cloudflare tunnel (optional)
│   ├── README.md
│   └── config.yml.example
│
├── db/migrations/             *.sql, run alphabetically on fresh DB
│   ├── 001_initial.sql
│   ├── 002_seed.sql           default admin user
│   ├── 003_multipart_parts.sql
│   ├── 004_s3_credentials.sql
│   ├── 005_system_config.sql
│   │   ... (intervening: object_versions, soft-delete, search, archival,
│   │        share_links, totp_2fa)
│   ├── 018_transcoded_bytes.sql            objects.transcoded_bytes column
│   ├── 019_transcode_quota_states.sql      adds skipped_quota, failed_quota
│   └── 020_transcode_reservation.sql       objects.transcode_reserved_bytes
│
├── scripts/                   end-to-end test scripts
│   ├── test-part3.sh          auth + quotas
│   ├── test-part4.sh          multipart upload
│   ├── test-part5.sh          transcoding
│   ├── test-part6.sh          nginx streaming
│   ├── test-part10.sh         aws-cli SigV4 (uses docker for aws-cli)
│   └── test-part10-boto.py    boto3 alternative
│
└── docs/                      ← you are here
    ├── architecture/          ascii diagrams: high-level, low-level, data-flows, schema
    ├── clients/               dashboard, curl, aws-cli, boto3, rclone
    ├── getting-started.md
    ├── cloudflare-tunnel.md
    ├── user-management.md
    ├── storage-management.md
    ├── api-reference.md
    ├── operations.md
    ├── troubleshooting.md
    └── development.md
```

## Development workflow

You don't need Go, Python, or Node installed locally — everything builds
inside Docker. But if you want hot reloads, install them.

### Iterate on the Go API

```bash
# Edit api/internal/handlers/objects.go (or wherever)
docker compose up -d --build api      # ~30s rebuild
docker compose logs -f api            # watch for crashes

# Quick verify
./scripts/test-part4.sh
```

To run with Go locally for fast iteration:
```bash
cd api
DATABASE_URL=postgres://s3admin:....@localhost:5432/personals3?sslmode=disable \
VALKEY_URL=redis://:....@localhost:6379/0 \
JWT_SECRET=$(grep JWT_SECRET ../.env | cut -d= -f2) \
STORAGE_ROOT=../storage \
API_PORT=3000 \
go run ./cmd/server
```

### Iterate on the worker

```bash
docker compose up -d --build worker
docker compose logs -f worker
```

Or local:
```bash
cd worker
python3 -m venv .venv && .venv/bin/pip install psycopg[binary] Pillow
DATABASE_URL=... LOG_LEVEL=DEBUG .venv/bin/python -m worker.main
```

### Iterate on the dashboard

For fast HMR (hot module reload), run Next.js locally:

```bash
cd dashboard
npm install
NEXT_PUBLIC_API_URL=http://localhost:8080/api npm run dev
# opens at http://localhost:3000 (Next default)
```

Then bypass the dashboard container in nginx by editing `nginx.conf` to
point at `host.docker.internal:3000` (Mac/Windows) or your host IP (Linux).

### Edit nginx config

```bash
# Edit nginx/nginx.conf
docker compose restart nginx
# Test
curl http://localhost:8080/nginx-health
```

---

## Adding a new feature

### Recipe: a new API endpoint

1. **Choose the file in `api/internal/handlers/`** — group by resource
   (auth.go, buckets.go, etc.) or make a new one.

2. **Write the handler:**
   ```go
   func (h *MyHandler) MyOp(w http.ResponseWriter, r *http.Request) {
       u := middleware.MustUser(r.Context())    // authenticated user
       // ... do thing ...
       httpx.WriteJSON(w, http.StatusOK, response)
   }
   ```

3. **Wire it in `cmd/server/main.go`:**
   ```go
   r.Group(func(r chi.Router) {
       r.Use(mw.Authenticator(pool, cfg.JWTSecret))
       // ...
       r.Get("/my-thing", myHandler.MyOp)
   })
   ```

4. **(If admin-only) put it under `r.Route("/admin", ...)` with `mw.AdminOnly`.**

5. **Update the audit middleware** if you want a clean action name in the
   audit log: `api/internal/middleware/audit.go` → `actionFromRoute()`.

6. **Add docs:** `docs/api-reference.md`.

### Recipe: a new database table

1. Write `db/migrations/006_xxx.sql`.
2. On a fresh DB it runs automatically. On an existing DB:
   ```bash
   docker compose exec -T postgres psql -U s3admin -d personals3 \
     < db/migrations/006_xxx.sql
   ```
3. Document in `docs/architecture/database-schema.md`.

### Recipe: a new transcode output format

1. Edit `worker/worker/video.py` (or audio.py / image.py)
2. Update `LADDER` constant or add a new ffmpeg call
3. Update the returned dict — that becomes `objects.transcoded` JSON
4. Update `dashboard/app/dashboard/buckets/[name]/page.tsx` `ObjectInfo`
   interface and rendering
5. `docker compose up -d --build worker`

### Recipe: a new dashboard page

1. New file at `dashboard/app/dashboard/something/page.tsx`
2. Add nav link in `dashboard/components/nav.tsx`
3. `docker compose up -d --build dashboard`

---

## Testing

We don't have unit tests (yet) — the test scripts in `scripts/` are
end-to-end smoke tests that exercise the whole stack via HTTP.

```bash
# Run all e2e tests in sequence
./scripts/test-part3.sh && \
  ./scripts/test-part4.sh && \
  ./scripts/test-part5.sh && \
  ./scripts/test-part6.sh && \
  /tmp/bv/bin/python ./scripts/test-part10-boto.py
```

Each one:
- Self-contained — starts from `docker compose up -d` state
- Cleans up after itself (deletes buckets/keys it created)
- Uses ANSI green/red output for pass/fail
- Exits non-zero on first failure

To add a new test:
- Copy an existing one as template
- Touch the new endpoint
- Verify both success and one failure mode

---

## Common gotchas while developing

### `docker compose up -d api` without `--build`

Doesn't rebuild the image, just restarts with the current image. To pick
up code changes, you MUST add `--build`:

```bash
docker compose up -d --build api
```

### Nginx config changes don't reload after `up -d`

Container spec didn't change, so nginx wasn't restarted. Force it:

```bash
docker compose restart nginx
```

### Migration didn't run

Migrations only run on a fresh database. Either wipe + restart:

```bash
docker compose down -v && docker compose up -d
```

Or apply manually:

```bash
docker compose exec -T postgres psql -U s3admin -d personals3 \
  < db/migrations/00X_my_migration.sql
```

### TypeScript build fails in Next.js

The dashboard build runs `tsc --noEmit` and fails on type errors. Common:

- `useEffect` cleanup must return `void`, not `boolean` (don't return
  `Set.delete()` directly — wrap it: `return () => { subs.delete(cb); }`)
- Layout files can't export anything except `default` and `metadata`

### Go can't find imports inside Docker

If `go.sum` is missing or stale, the build fails. Our Dockerfile runs
`go mod tidy` first to regenerate it.

---

## What's not built yet

Sensible next features (in rough priority order):

| Feature | Effort | Impact |
|---|---|---|
| Object versioning | Medium | Compliance, "oops I deleted it" |
| Presigned URLs | Low | Sharing links without exposing API keys |
| Lifecycle rules | Medium | Auto-archive old data |
| Bucket public-read mode | Low | Direct sharing without auth |
| Cron-based cleanup of orphaned segments and expired multipart uploads | Low | Disk reclaim |
| Password-change UI for non-admin users | Low | UX gap |
| Multi-region tracking | High | "store this on the SSD, that on the HDD" |
| Inline file editor for text files in dashboard | Low | Nice-to-have |
| WebSocket-based real-time upload progress (right now we poll) | Medium | UX |
| OIDC / SAML SSO | High | Enterprise auth |

Pull requests welcome. Open a discussion first for the bigger items.
