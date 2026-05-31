# Python (boto3)

```bash
pip install boto3
```

## Boilerplate

```python
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="https://s3.yourdomain.com/api",      # YOUR HOST + /api
    aws_access_key_id="AKIAXXXXXXXXXXXXXXXX",
    aws_secret_access_key="xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    region_name="us-east-1",
    config=Config(
        signature_version="s3v4",
        s3={"addressing_style": "path"},                # required (no virtual-hosted)
    ),
)
```

You can pass credentials via env vars too:

```python
# AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_DEFAULT_REGION
import os, boto3
from botocore.config import Config

s3 = boto3.client("s3",
    endpoint_url=os.environ["PS3_URL"] + "/api",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}))
```

## Recipes

### List buckets, create, delete

```python
r = s3.list_buckets()
print([b["Name"] for b in r["Buckets"]])

s3.create_bucket(Bucket="my-photos")
s3.delete_bucket(Bucket="my-photos")
```

### Upload / download files

```python
# Simple
s3.upload_file("/tmp/cat.jpg", "my-photos", "pets/cat.jpg")
s3.download_file("my-photos", "pets/cat.jpg", "/tmp/restored.jpg")

# From memory / bytes
s3.put_object(
    Bucket="my-photos",
    Key="manifest.json",
    Body=b'{"version":1}',
    ContentType="application/json",
)

# Read into memory
r = s3.get_object(Bucket="my-photos", Key="manifest.json")
data = r["Body"].read()
print(data)

# Stream a large download (chunked)
r = s3.get_object(Bucket="my-photos", Key="big.zip")
with open("/tmp/big.zip", "wb") as f:
    for chunk in r["Body"].iter_chunks(chunk_size=1024 * 1024):
        f.write(chunk)
```

### Multipart upload (automatic for files > 8 MiB)

```python
from boto3.s3.transfer import TransferConfig

config = TransferConfig(
    multipart_threshold=8 * 1024 * 1024,
    max_concurrency=4,
    multipart_chunksize=5 * 1024 * 1024,
    use_threads=True,
)

s3.upload_file(
    "/path/to/huge.mp4",
    "videos",
    "huge.mp4",
    Config=config,
    Callback=lambda n: print(f"\rsent {n} bytes", end=""),
)
```

### List objects with pagination

```python
paginator = s3.get_paginator("list_objects_v2")
pages = paginator.paginate(Bucket="my-photos", Prefix="2024/")
for page in pages:
    for obj in page.get("Contents", []):
        print(obj["Key"], obj["Size"], obj["LastModified"])
```

### Check existence

```python
from botocore.exceptions import ClientError

def exists(bucket, key):
    try:
        s3.head_object(Bucket=bucket, Key=key)
        return True
    except ClientError as e:
        if e.response["Error"]["Code"] in ("404", "NoSuchKey", "NotFound"):
            return False
        raise

print(exists("my-photos", "cat.jpg"))
```

### Copy server-side (avoids download+reupload)

```python
s3.copy_object(
    Bucket="dest-bucket",
    Key="copied.jpg",
    CopySource={"Bucket": "my-photos", "Key": "cat.jpg"},
)
```

*(Note: this is partially supported in PersonalS3 — currently it routes
through the standard PutObject path but with the source data re-read by
the server. True CopyObject is on the roadmap.)*

### Delete

```python
s3.delete_object(Bucket="my-photos", Key="cat.jpg")

# Bulk delete
s3.delete_objects(
    Bucket="my-photos",
    Delete={"Objects": [{"Key": "a.jpg"}, {"Key": "b.jpg"}]},
)
```

## Backup script (Python)

```python
#!/usr/bin/env python3
"""Sync a local dir to PersonalS3, daily via cron."""
import os
import boto3
from botocore.config import Config
from pathlib import Path
from datetime import date

SRC = Path("/home/me/important")
BUCKET = "daily-backup"
TODAY = date.today().isoformat()

s3 = boto3.client("s3",
    endpoint_url="https://s3.yourdomain.com/api",
    aws_access_key_id=os.environ["PS3_AKID"],
    aws_secret_access_key=os.environ["PS3_SECRET"],
    region_name="us-east-1",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}))

for path in SRC.rglob("*"):
    if not path.is_file():
        continue
    rel = path.relative_to(SRC)
    key = f"{TODAY}/{rel}"
    s3.upload_file(str(path), BUCKET, key)
    print(f"  {rel}")
```

## See also

- [aws-cli.md](./aws-cli.md) — same auth, command-line equivalent
- [../api-reference.md](../api-reference.md) — what's implemented
