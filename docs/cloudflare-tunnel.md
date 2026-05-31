# Cloudflare Tunnel — Using Your Existing Tunnel

If you already have a Cloudflare tunnel configured (you add local services to
it via the Cloudflare dashboard's "Public Hostname" section), here's how to
add PersonalS3 to it. **Two minutes of work.**

---

## TL;DR

In your Cloudflare Zero Trust dashboard:

> Networks → Tunnels → click your tunnel → **Public Hostname** tab →
> **Add a public hostname**

Fill in:

| Field | Value |
|---|---|
| **Subdomain** | `s3` (or `storage`, `files`, whatever you prefer) |
| **Domain** | your-domain.com |
| **Service — Type** | `HTTP` |
| **Service — URL** | `localhost:8080` |

Save. Wait 10 seconds for the tunnel to propagate. Open
`https://s3.your-domain.com`. Done.

---

## What "Service URL" to use

Your `cloudflared` process is running in one of these places. Match the
right value:

| Where cloudflared runs | Service URL to use |
|---|---|
| **On the host** (systemd, `cloudflared service install`) | `http://localhost:8080` |
| **In Docker, on the same Docker network as nginx** (e.g. via the optional `cloudflared` service in our compose) | `http://nginx:80` |
| **In Docker, with `--network=host`** | `http://localhost:8080` |
| **In Docker on a separate network** | `http://host.docker.internal:8080` (Mac/Win) or `http://172.17.0.1:8080` (Linux Docker bridge IP) |

If unsure, the host-based one (`http://localhost:8080`) almost always works
because most people install cloudflared as a systemd service.

---

## Why HTTP (not HTTPS) for the Service URL?

TLS terminates at Cloudflare. The tunnel between cloudflared and your
service is already encrypted (it's a private connection over Cloudflare's
network). Using HTTPS internally would just add CPU overhead for no
security gain — and you'd need a self-signed cert.

---

## After it's working

Update your dashboard to use the public URL so the API base matches what the
browser actually loaded:

```bash
# In ~/Projects/S3orSimilar/.env, change:
NEXT_PUBLIC_API_URL=/api               # same-origin works for tunnel too

# If you want to use a separate origin for the API (e.g. you set up
# api.your-domain.com pointing at the same nginx), set:
# NEXT_PUBLIC_API_URL=https://api.your-domain.com/api
```

Then rebuild the dashboard:

```bash
docker compose up -d --build dashboard
```

---

## Splitting into multiple subdomains (optional)

If you want cleaner separation:

| Subdomain | Service URL | What it serves |
|---|---|---|
| `s3.your-domain.com`     | `http://localhost:8080` | Everything (dashboard + API + streams) |
| `dash.your-domain.com`   | `http://localhost:8080` | Same (browser hits `/`, gets dashboard) |
| `api.your-domain.com`    | `http://localhost:8080` | Same (then call `/api/...` from clients) |
| `stream.your-domain.com` | `http://localhost:8080` | Same (call `/stream/...` for HLS) |

All four point to the same nginx — the path determines what's served.

---

## Quick sanity check

After saving the hostname in Cloudflare:

```bash
# From any device on the internet (or your phone on 5G, not on your WiFi):
curl https://s3.your-domain.com/nginx-health
# → "ok"

# Hit the API
curl https://s3.your-domain.com/api/healthz
# → "ok"

# Login from the public URL
curl -X POST https://s3.your-domain.com/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@local","password":"YOUR-CHANGED-PASSWORD"}'
```

If those work, the whole stack is live globally.

---

## What this gives you

- **Public HTTPS URL** with a real cert, no self-signed warnings
- **Hidden home IP** — Cloudflare's IP is what the world sees
- **DDoS protection** at the edge, free
- **Globally cached HLS segments** (`/stream/*.ts` is `Cache-Control: immutable, max-age=1y`)
  — viewers in Asia pull from Cloudflare's nearest POP, not your laptop
- **No port forwarding** — your home router stays locked down
- **Works behind CGNAT** — the tunnel is outbound from your machine

---

## Troubleshooting

**Cloudflare shows `Bad Gateway` or `Error 502`**
→ cloudflared can't reach the service. Check the Service URL is correct
   for where cloudflared runs. Test from the cloudflared host:
   `curl http://localhost:8080/nginx-health`.

**`Cloudflare Error 1033: Argo Tunnel error`**
→ Tunnel isn't running. On the host:
   `sudo systemctl restart cloudflared` (or for Docker: `docker compose
   restart cloudflared`).

**Browser shows dashboard but API calls fail with CORS errors**
→ The dashboard is configured for a different origin than what your
   browser is hitting. Set `NEXT_PUBLIC_API_URL=/api` (relative) in `.env`,
   rebuild the dashboard.

**Upload via tunnel feels slow**
→ Large uploads go through cloudflared then out to Cloudflare. Inbound is
   bottlenecked by your home upload bandwidth. Once at Cloudflare, the
   data flows to the rest of the world at edge speed. Test your upstream:
   `speedtest-cli`.

---

## Removing PersonalS3 from your tunnel

In Cloudflare Zero Trust → your tunnel → Public Hostname → click the row
for `s3.your-domain.com` → Delete.

Or to disable temporarily, stop the stack on your laptop:
```bash
docker compose down
```
Visitors will see `Bad Gateway` until you bring it back.
