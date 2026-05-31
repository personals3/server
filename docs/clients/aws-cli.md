# AWS CLI

PersonalS3 speaks AWS Signature V4, so the official `aws` CLI works
unchanged — just add `--endpoint-url`.

## Setup (once per user)

1. Dashboard → `/dashboard/keys` → **S3 Credentials** → Create →
   copy AKID + secret (shown once)

2. Configure a profile:

   ```bash
   aws configure --profile personals3
   # AWS Access Key ID:     AKIAXXXXXXXXXXXXXXXX
   # AWS Secret Access Key: xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
   # Default region name:   us-east-1
   # Default output:        json
   ```

3. Alias for convenience:

   ```bash
   alias ps3='aws --profile personals3 --endpoint-url=https://s3.yourdomain.com/api'
   ```

(For local-only: `--endpoint-url=http://localhost:8080/api`)

## Daily-use commands

### List buckets
```bash
ps3 s3 ls
```

### Create / delete bucket
```bash
ps3 s3 mb s3://my-bucket
ps3 s3 rb s3://my-bucket
ps3 s3 rb s3://my-bucket --force         # delete contents first
```

### Upload
```bash
ps3 s3 cp myfile.txt s3://my-bucket/myfile.txt
ps3 s3 cp ./somedir s3://my-bucket/ --recursive
```

### Download
```bash
ps3 s3 cp s3://my-bucket/myfile.txt ./
ps3 s3 cp s3://my-bucket/ ./localdir --recursive
```

### List objects
```bash
ps3 s3 ls s3://my-bucket/
ps3 s3 ls s3://my-bucket/ --recursive
ps3 s3 ls s3://my-bucket/photos/2024/
```

### Sync (the killer feature for backups)
```bash
# Push local → remote
ps3 s3 sync ~/Documents s3://docs-backup/

# Pull remote → local
ps3 s3 sync s3://docs-backup/ ~/Documents-restored

# Delete files in destination that aren't in source
ps3 s3 sync ~/Documents s3://docs-backup/ --delete

# Exclude patterns
ps3 s3 sync ~/Projects s3://code-backup/ \
  --exclude "*/node_modules/*" \
  --exclude "*/.next/*" \
  --exclude "*.log"
```

### Delete
```bash
ps3 s3 rm s3://my-bucket/myfile.txt
ps3 s3 rm s3://my-bucket/somedir/ --recursive
```

### Move (copy + delete)
```bash
ps3 s3 mv s3://my-bucket/old.txt s3://my-bucket/new.txt
```

## Backup script template

```bash
#!/bin/bash
# /home/me/bin/backup-home.sh — run nightly via cron
set -e

PROFILE=personals3
ENDPOINT=https://s3.yourdomain.com/api
BUCKET=daily-backup
SRC=/home/me/important

DATE=$(date +%F)
aws --profile $PROFILE --endpoint-url=$ENDPOINT s3 sync \
  "$SRC" "s3://$BUCKET/$DATE/" \
  --exclude "*.tmp" \
  --exclude "*/cache/*" \
  --no-progress 2>&1 | tail -10

echo "Backup of $SRC complete: $(date)"
```

Crontab:
```cron
0 3 * * * /home/me/bin/backup-home.sh >> /var/log/personals3-backup.log 2>&1
```

## Limitations

These S3 features are **not** implemented yet — calls will return errors:

- `aws s3api put-bucket-versioning` — no versioning
- `aws s3api put-bucket-lifecycle-configuration` — no lifecycle
- `aws s3 presign` — no presigned URLs
- `aws s3api put-bucket-policy` — no bucket policies (use per-user quotas instead)

These S3 features **work**:

- Bucket CRUD, object CRUD, listing
- Multipart upload (boto3/aws-cli switches automatically for files > 8 MiB)
- Range requests on download
- ETags (single-part = MD5, multipart = MD5-of-MD5s-N format)
- Standard storage class (it's the only one)

## See also

- [boto3.md](./boto3.md) for Python applications
- [rclone.md](./rclone.md) for richer sync features
- [../api-reference.md](../api-reference.md) for the full endpoint list
