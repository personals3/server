# High-Level Architecture

PersonalS3 is **seven containers** orchestrated by Docker Compose, sitting
behind one nginx that fronts the whole system.

```
                              The Internet
                                   │
                                   │ HTTPS
                                   ▼
                  ┌────────────────────────────────────┐
                  │       Cloudflare edge              │
                  │   • TLS termination + auto-renew   │
                  │   • DDoS / WAF                     │
                  │   • Caches /stream/*.ts segments   │
                  │     at 300+ POPs worldwide         │
                  └────────────────┬───────────────────┘
                                   │ encrypted persistent
                                   │ outbound tunnel (cloudflared)
                                   ▼
   ┌─────────────────────────────────────────────────────────────┐
   │                YOUR LAPTOP / HOMELAB                        │
   │                                                             │
   │  ┌─────────────────┐                                        │
   │  │  cloudflared    │  (optional — only if you want public   │
   │  │  (container)    │   access; can be skipped for LAN-only) │
   │  └────────┬────────┘                                        │
   │           │                                                 │
   │           ▼                                                 │
   │  ┌──────────────────────────────────────────────────────┐   │
   │  │              nginx     (port 8080)                   │   │
   │  │                                                      │   │
   │  │   /                  →  Next.js dashboard            │   │
   │  │   /api/*             →  Go API (strips /api prefix)  │   │
   │  │   /stream/{id}/...   →  static HLS/image via sendfile│   │
   │  │   /nginx-health      →  "ok"                         │   │
   │  └──┬───────────────┬───────────────────┬──────────────┘   │
   │     │               │                   │                   │
   │     ▼               ▼                   ▼                   │
   │  ┌──────────┐  ┌──────────┐    ┌────────────────────┐       │
   │  │ Next.js  │  │  Go API  │    │  storage/          │       │
   │  │ dashboard│  │  (chi    │    │  (bind-mounted     │       │
   │  │ (3001)   │  │  router) │    │   from host disk)  │       │
   │  │          │  │  (3000)  │    │                    │       │
   │  └──────────┘  └────┬─────┘    │  buckets/{id}/     │       │
   │                     │          │  segments/{id}/    │       │
   │              ┌──────┴──────┐   └────────────────────┘       │
   │              ▼             ▼                                │
   │      ┌─────────────┐  ┌─────────────┐                       │
   │      │ PostgreSQL  │  │   Valkey    │                       │
   │      │  (5432)     │  │   (6379)    │                       │
   │      │  metadata + │  │  rate limits│                       │
   │      │  quotas +   │  │  + ephemeral│                       │
   │      │  audit +    │  │  state      │                       │
   │      │  jobs       │  │             │                       │
   │      └──────┬──────┘  └─────────────┘                       │
   │             │                                               │
   │             │ FOR UPDATE SKIP LOCKED                        │
   │             ▼                                               │
   │      ┌─────────────────────────────┐                        │
   │      │  Python worker(s)           │                        │
   │      │  + FFmpeg + Pillow          │                        │
   │      │  → HLS .m3u8 + .ts          │                        │
   │      │  → WebP / AVIF / MP3 / OGG  │                        │
   │      └─────────────────────────────┘                        │
   │                                                             │
   └─────────────────────────────────────────────────────────────┘
```

## Why each piece exists

| Component | Why it's there | What dies if you remove it |
|---|---|---|
| **PostgreSQL** | Atomic quota accounting; SQL transactions prevent double-spending storage; queue table for transcoding jobs | All metadata, quotas, audit log |
| **Valkey** | Sub-millisecond rate-limit counters; ephemeral session state. Redis-compatible, fully open source. | Rate limiting; (sessions still survive in JWT) |
| **Go API** | Streaming I/O for huge uploads with low memory; single static binary; native concurrency. | Everything — this is the brain |
| **Python worker** | FFmpeg + Pillow are mature in Python; easy for you (or me) to extend with new output formats | Media transcoding (uploads still work, just no HLS) |
| **Nginx** | Zero-copy `sendfile()` for HLS segments (~10× faster than Go would do); single front door; CORS handling | Streaming performance; single-origin convenience |
| **Next.js dashboard** | Browser UI; React Server Components; embedded video.js player | Web UI (API and CLI clients still work) |
| **cloudflared** | Outbound-only tunnel — no port forwarding, no static IP, free auto-TLS | Public internet access (LAN access still works) |

## What this is, vs what it isn't

| ✅ What PersonalS3 is | ❌ What it isn't |
|---|---|
| Personal/team-scale S3 (1 disk, dozens of users) | Hyperscale (we have no multi-region, no erasure coding) |
| Built-in media pipeline | A general workflow engine |
| S3-compatible for the common subset | A drop-in for AWS S3 (no versioning, lifecycle, presigned URLs yet) |
| Single-machine | Distributed/HA — there's one PostgreSQL, one disk |
| Open and inspectable | A commercial SaaS — you run it yourself |

## See also

- [low-level.md](./low-level.md) for each component's internals
- [data-flows.md](./data-flows.md) for what a request actually does
- [../getting-started.md](../getting-started.md) to run this thing
