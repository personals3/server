# Getting Started

5 minutes from `git clone` to a working dashboard.

## Prerequisites

- Linux or macOS host
- Docker 20+ with Compose v2 (`docker compose version` must work)
- ~3 GB free disk for container images, plus whatever you'll store
- Your shell user must be in the `docker` group:
  ```bash
  sudo usermod -aG docker $USER
  newgrp docker        # picks up the new group without logout
  ```

## First-time setup

```bash
cd ~/Projects/S3orSimilar           # this repository

# 1. Copy the env template and edit at least the passwords
cp .env.example .env
# Edit these in .env:
#   POSTGRES_PASSWORD     ← change from "changeme..."
#   VALKEY_PASSWORD       ← change
#   JWT_SECRET            ← run: openssl rand -hex 32

# 2. Make sure UID/GID match (they default to 1000:1000):
id -u   # should be 1000 on most Linux systems
id -g   # should be 1000
# If yours differ, edit UID= and GID= in .env

# 3. Pick your storage location
# Default: ./storage (in the project directory)
# For real use, point to a bigger disk:
#   STORAGE_ROOT=/mnt/bigdisk/personals3-data
mkdir -p "$STORAGE_ROOT"           # make sure it exists

# 4. Start everything
docker compose up -d --build       # first build ~3-5 min
```

Verify it's healthy:

```bash
docker compose ps
# All services should show "Up" and most "(healthy)"
```

Open **http://localhost:8080** in a browser.

| Field | Default |
|---|---|
| Email | `admin@local` |
| Password | `admin` |

**Immediately change the admin password** via Admin → Users → click yourself.

## Sanity tests

```bash
./scripts/test-part3.sh       # auth + quotas (≈10 seconds)
./scripts/test-part4.sh       # multipart upload (≈30 seconds)
./scripts/test-part6.sh       # nginx streaming (≈15 seconds)
```

If those all show "All Part X tests passed", you're good.

## What's running

```
$ docker compose ps
NAME           SERVICE      PORTS
s3-postgres    postgres     0.0.0.0:5432
s3-valkey      valkey       0.0.0.0:6379
s3-api         api          0.0.0.0:3000
s3-worker-1    worker       (internal)
s3-nginx       nginx        0.0.0.0:8080      ← this is the front door
s3-dashboard   dashboard    0.0.0.0:3001      ← also reachable directly
```

In day-to-day use you only ever touch **port 8080** (nginx) — everything routes
through it: dashboard at `/`, API at `/api/*`, streams at `/stream/*`.

The other ports are open mainly for debugging.

## Next steps

- [user-management.md](./user-management.md) — create accounts for other people
- [cloudflare-tunnel.md](./cloudflare-tunnel.md) — expose to the internet
- [storage-management.md](./storage-management.md) — manage disk capacity and quotas
- [clients/](./clients/) — use it from CLI / Python / boto3 / rclone

## Stop / restart / reset

```bash
# Stop containers, keep data
docker compose down

# Stop AND blow away all data (DB, Valkey, ALL UPLOADED FILES via volume)
docker compose down -v

# Restart a single service after editing its code
docker compose up -d --build api

# View live logs from a service
docker compose logs -f api
docker compose logs -f worker

# Shell into a container
docker compose exec postgres psql -U s3admin -d personals3
docker compose exec api sh
```

## Common first-run problems

| Symptom | Likely cause | Fix |
|---|---|---|
| `permission denied while trying to connect to the Docker API` | Your user is not in the `docker` group | `sudo usermod -aG docker $USER && newgrp docker` |
| API container in restart loop, logs say `mkdir /storage/buckets: permission denied` | `UID`/`GID` in `.env` don't match the owner of `STORAGE_ROOT` | Fix `.env` UID/GID, `docker compose up -d` |
| `bind: address already in use` on 8080 | Something else is on port 8080 | Edit `NGINX_HTTP_PORT=8081` in `.env`, restart |
| Login fails with `invalid credentials` | Password not what you expected | `docker compose down -v && docker compose up -d` resets to `admin@local` / `admin` |
| Dashboard loads but says `Loading...` forever | Browser cached old JS pointing at port 3000 | Hard refresh (`Ctrl+Shift+R`) |

For more, see [troubleshooting.md](./troubleshooting.md).
