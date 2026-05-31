# Deploying to a Real Server

Step-by-step for taking this from your laptop to a production box.

## On the server

```bash
# 1. Prerequisites
sudo apt update
sudo apt install -y docker.io docker-compose-plugin git
sudo usermod -aG docker $USER
# log out + back in (or `newgrp docker`)

# 2. Clone
git clone https://github.com/YOUR_USERNAME/S3orSimilar.git
cd S3orSimilar

# 3. Generate fresh secrets and admin credentials
./scripts/init-secrets.sh

# 4. Start the stack
docker compose up -d --build

# 5. Verify
docker compose ps                             # all services should be "(healthy)" or "Up"
curl http://localhost:8080/nginx-health       # → "ok"
curl http://localhost:8080/api/healthz        # → "ok"

# 6. Open http://localhost:8080 (or via your reverse proxy) and log in
#    with the credentials init-secrets printed.
```

## Adding a Cloudflare Tunnel hostname

In Cloudflare Zero Trust → Networks → Tunnels → your tunnel → **Public Hostname** → **Add**:

| Field | Value |
|---|---|
| Subdomain | `s3` (or whatever) |
| Domain | yourdomain.com |
| Service type | `HTTP` |
| Service URL | `localhost:8080` (or `nginx:80` if cloudflared runs in our compose) |

Save → open `https://s3.yourdomain.com` from anywhere.

See [cloudflare-tunnel.md](./cloudflare-tunnel.md) for the full guide.

## Pre-flight checklist (do these BEFORE making it public)

- [ ] `init-secrets.sh` was run — strong random `POSTGRES_PASSWORD`, `VALKEY_PASSWORD`, `JWT_SECRET`
- [ ] `ADMIN_PASSWORD` in `.env` is not `admin`
- [ ] `.env` is in `.gitignore` (it is by default) — `git status` should not list it
- [ ] `.env` file permissions are `600`: `chmod 600 .env`
- [ ] Server's firewall blocks 3000, 3001, 5432, 6379 from the outside (they're already loopback-only in compose)
- [ ] If using the Cloudflare quick tunnel, do **not** rely on the URL staying secret — anyone who learns it can hit your dashboard. Use the named tunnel + a real password.
- [ ] Test the storage path has enough free space: `df -h $(grep STORAGE_ROOT .env | cut -d= -f2)`
- [ ] Backup plan: at minimum, schedule `pg_dump` of the DB to another disk. See [operations.md](./operations.md).
- [ ] (Optional) Set up monitoring: hit `/api/admin/stats` and alert on `disk.physicalUsed > 80%`

## Upgrading

```bash
cd S3orSimilar
git pull
docker compose down              # stops services, KEEPS DATA
docker compose up -d --build     # rebuilds images, picks up code changes

# If a new SQL migration was added (db/migrations/00X_*.sql), apply against
# the existing DB (the file in docker-entrypoint-initdb.d only runs on first
# startup):
docker compose exec -T postgres psql -U s3admin -d personals3 \
  < db/migrations/00X_new_migration.sql
```

## Backups

The system has two kinds of state — back up both:

### Database (`s3-postgres`)
```bash
docker compose exec -T postgres pg_dump -U s3admin -d personals3 \
  | gzip > /mnt/backups/personals3-db-$(date +%F).sql.gz
```

### Object data (`storage/`)
```bash
rsync -av --delete $STORAGE_ROOT /mnt/backups/personals3-data/
```

Schedule both as a nightly cron. To restore: stop the stack, restore both, start.

## What `init-secrets.sh` doesn't do

- **TLS certs** — Cloudflare provides them for tunneled hostnames automatically.
  If you're skipping Cloudflare and exposing nginx directly, terminate TLS with
  another reverse proxy in front (Caddy / nginx with certbot).
- **Backups** — see Operations doc.
- **Resource limits** — add `mem_limit` / `cpus` in compose if you're sharing
  the box with other workloads.

## What you'll want to change later

These are intentional defaults that aren't quite right for production:

| Default | Where | Suggested production value |
|---|---|---|
| Worker count = 1 | compose `worker.scale` (or `--scale worker=N`) | `nproc / 2` |
| Per-user quota 10 GB | `.env` `DEFAULT_QUOTA_BYTES` | Whatever fits your disk |
| Rate limit 1000 req/min | hard-coded in `api/cmd/server/main.go` `mw.RateLimit(rdb, 1000)` | Adjust to taste |
| Disk-full threshold 95% | `Admin → System → Storage` UI | 90% if growth is bursty |
