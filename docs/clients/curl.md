# curl / shell scripts

Use the native API with Bearer tokens — simpler than SigV4, no signature math.

## Setup

```bash
# One-time: get an API key from dashboard
#   /dashboard/keys → Bearer API Keys → Create
# Or via login + create:
JWT=$(curl -s -X POST https://s3.yourdomain.com/api/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"you@example.com","password":"..."}' \
  | jq -r .token)

KEY=$(curl -s -X POST https://s3.yourdomain.com/api/auth/keys \
  -H "Authorization: Bearer $JWT" \
  -d '{"name":"my-script"}' \
  | jq -r .key)

# Save to env file
cat > ~/.personals3.env <<EOF
export PS3_URL="https://s3.yourdomain.com/api"
export PS3_KEY="$KEY"
EOF
chmod 600 ~/.personals3.env
```

## Pattern

```bash
source ~/.personals3.env

curl -H "Authorization: Bearer $PS3_KEY" "$PS3_URL/..."
```

## Recipes

### List your buckets
```bash
curl -H "Authorization: Bearer $PS3_KEY" "$PS3_URL/" | jq
```

### Create a bucket
```bash
curl -X PUT -H "Authorization: Bearer $PS3_KEY" "$PS3_URL/my-bucket"
```

### Upload a file
```bash
curl -X PUT "$PS3_URL/my-bucket/path/to/file.txt" \
  -H "Authorization: Bearer $PS3_KEY" \
  -H "Content-Type: text/plain" \
  --data-binary @file.txt
```

Keys can contain slashes — they're part of the key name, not real directories.

### Upload a big file (multipart)
The native API supports multipart but it's verbose. For big files, prefer
[aws-cli](./aws-cli.md) or [rclone](./rclone.md). If you must:

```bash
# 1. Initiate
UID=$(curl -sX POST "$PS3_URL/my-bucket/big.zip?uploads" \
  -H "Authorization: Bearer $PS3_KEY" \
  -H "Content-Type: application/zip" | jq -r .uploadId)

# 2. Split + upload parts
split -b 5M big.zip part_
ETAGS='[]'
N=1
for p in part_*; do
  ETAG=$(curl -sX PUT "$PS3_URL/my-bucket/big.zip?partNumber=$N&uploadId=$UID" \
    -H "Authorization: Bearer $PS3_KEY" \
    --data-binary @"$p" -D - | grep -i ^etag | awk '{print $2}' | tr -d '"\r')
  ETAGS=$(echo "$ETAGS" | jq --argjson n "$N" --arg e "$ETAG" \
    '. + [{partNumber:$n, etag:$e}]')
  N=$((N + 1))
done

# 3. Complete
curl -X POST "$PS3_URL/my-bucket/big.zip?uploadId=$UID" \
  -H "Authorization: Bearer $PS3_KEY" \
  -H "Content-Type: application/json" \
  -d "$(echo "$ETAGS" | jq '{parts:.}')"
```

### Download a file
```bash
curl "$PS3_URL/my-bucket/file.txt" \
  -H "Authorization: Bearer $PS3_KEY" \
  -o file.txt
```

### Range download (partial)
```bash
# Get bytes 0-1023 (first 1KB)
curl "$PS3_URL/my-bucket/big.zip" \
  -H "Authorization: Bearer $PS3_KEY" \
  -H "Range: bytes=0-1023" \
  -o first-kb.bin
```

### Check if an object exists (HEAD)
```bash
curl -sI "$PS3_URL/my-bucket/file.txt" \
  -H "Authorization: Bearer $PS3_KEY" | head -1
# → HTTP/1.1 200 OK   (or 404)
```

### List objects in a bucket
```bash
curl "$PS3_URL/my-bucket" -H "Authorization: Bearer $PS3_KEY" | jq

# With prefix filter
curl "$PS3_URL/my-bucket?prefix=photos/2024/" \
  -H "Authorization: Bearer $PS3_KEY" | jq

# Pagination (max-keys, max 1000)
curl "$PS3_URL/my-bucket?max-keys=100" \
  -H "Authorization: Bearer $PS3_KEY" | jq
```

### Delete
```bash
# Object
curl -X DELETE "$PS3_URL/my-bucket/file.txt" \
  -H "Authorization: Bearer $PS3_KEY"

# Bucket (must be empty)
curl -X DELETE "$PS3_URL/my-bucket" -H "Authorization: Bearer $PS3_KEY"
```

### See yourself
```bash
curl "$PS3_URL/auth/me" -H "Authorization: Bearer $PS3_KEY" | jq
# {"email":"you@example.com","quotaBytes":..., "usedBytes":...}
```

### Stream upload from another process
```bash
# Upload tar of a directory without writing to disk
tar -cf - my-dir/ | \
  curl -X PUT "$PS3_URL/backups/my-dir-$(date +%F).tar" \
    -H "Authorization: Bearer $PS3_KEY" \
    -H "Content-Type: application/x-tar" \
    --data-binary @-
```

## Watch out for

- **Rate limit:** 1000 req/min per user (10000 for admins). Hit → `429 Too Many Requests` with `Retry-After: 60`.
- **Quota:** Upload would exceed your quota → `507 Insufficient Storage`.
- **Disk full:** Whole-system disk past `disk_full_threshold_pct` → `507 Insufficient Storage` with `code: DISK_FULL`.

Check rate-limit headers on every response:

```
X-RateLimit-Limit:     1000
X-RateLimit-Remaining: 994
```
