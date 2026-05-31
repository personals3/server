# Cloudflare Tunnel Setup

Two modes are configured in `docker-compose.yml` via profiles. Pick one.

---

## Mode A — Quick Tunnel (zero setup)

Use this for testing or showing someone a demo. No Cloudflare account needed.

```bash
cd ~/Projects/S3orSimilar
docker compose --profile quick-tunnel up -d cloudflared-quick

# Wait a few seconds, then grab your URL
docker compose logs cloudflared-quick | grep trycloudflare.com
# →  https://abcd-random-words.trycloudflare.com
```

Your full stack (dashboard, API, /stream HLS) is now publicly accessible at
that URL. The URL changes every restart.

To stop:
```bash
docker compose --profile quick-tunnel down
```

---

## Mode B — Named Tunnel (your own domain)

Persistent custom URL like `s3.yourdomain.com`. Requires a domain hosted on
Cloudflare (the domain's nameservers must point to Cloudflare).

### One-time setup

1. **Add your domain to Cloudflare** (skip if already done):
   - dash.cloudflare.com → Add a Site
   - Follow the nameserver instructions at your registrar

2. **Create a tunnel:**
   - dash.cloudflare.com → Zero Trust → Networks → Tunnels
   - Click **Create a tunnel** → **Cloudflared** → name it "personal-s3"
   - **Copy the token** shown on the install screen (long `eyJ...` string)

3. **Save the token:**
   ```bash
   # In ~/Projects/S3orSimilar/.env
   CLOUDFLARED_TOKEN=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...
   ```

4. **Configure public hostnames** (still in the Cloudflare web UI):
   - On the tunnel page → **Public Hostname** tab → **Add a public hostname**
   - Configure these three (one per row):

   | Subdomain  | Domain         | Service Type | URL              |
   |------------|----------------|--------------|------------------|
   | `s3`       | yourdomain.com | HTTP         | `nginx:80`       |
   | `dash`     | yourdomain.com | HTTP         | `dashboard:3001` |

   Or use a single hostname (`s3.yourdomain.com`) for everything — the
   dashboard talks to the API at the same origin through the nginx proxy.

5. **Start the tunnel:**
   ```bash
   docker compose --profile tunnel up -d cloudflared
   docker compose logs -f cloudflared
   # → look for "Registered tunnel connection" lines
   ```

### Verify

```bash
# API works
curl https://s3.yourdomain.com/healthz

# Dashboard loads
open https://dash.yourdomain.com
```

### Update the dashboard's API URL

Since the dashboard calls the API through the browser, set its base URL
in `.env`:

```bash
NEXT_PUBLIC_API_URL=https://s3.yourdomain.com
```

Then rebuild the dashboard:
```bash
docker compose up -d --build dashboard
```

---

## Troubleshooting

**`Authorization failed` in cloudflared logs**
→ The token in `.env` is wrong or expired. Re-copy it from the Cloudflare UI.

**Tunnel up but URL returns 502 Bad Gateway**
→ Public hostname in Cloudflare is pointing to the wrong service. Edit it to
   `http://nginx:80` (HTTP, not HTTPS — TLS terminates at Cloudflare, the
   internal hop is plain HTTP).

**Dashboard loads but API calls fail (CORS error)**
→ You're hitting the dashboard at one URL but the dashboard is configured
   to call the API at another. Set `NEXT_PUBLIC_API_URL` to match what your
   browser actually loads, then rebuild the dashboard.
