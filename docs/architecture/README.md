# Architecture

Four documents, read in order:

1. **[high-level.md](./high-level.md)** — 30,000-ft view. The 7 containers, why each is here, what dies if you remove it.
2. **[low-level.md](./low-level.md)** — every container in detail: its image, port, volumes, code layout, request lifecycle.
3. **[data-flows.md](./data-flows.md)** — six end-to-end sequence diagrams: login, small upload, multipart upload, transcoding, HLS playback, SigV4.
4. **[database-schema.md](./database-schema.md)** — ER diagram + every table column-by-column.

## Quick context

- **Stack:** Go (API) + Python (transcode worker) + Next.js (dashboard) + PostgreSQL + Valkey + Nginx
- **Storage layout:** SHA-256-hashed object keys, atomic writes via temp-rename, named volumes for DB
- **Auth:** three mutually-exclusive methods (JWT for dashboard, Bearer API keys for scripts, AWS SigV4 for boto3/aws-cli)
- **Transcoding:** PostgreSQL queue table polled with `FOR UPDATE SKIP LOCKED` — many workers scale linearly
- **Streaming:** Nginx serves HLS segments via `sendfile()` with edge-friendly cache headers
