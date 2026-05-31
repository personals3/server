#!/usr/bin/env python3
"""Part 10 verification using boto3 instead of aws-cli (clearer errors).

Run: pip3 install --break-system-packages boto3 && ./scripts/test-part10-boto.py
or:  python3 -m venv /tmp/bv && /tmp/bv/bin/pip install boto3 && /tmp/bv/bin/python ./scripts/test-part10-boto.py
"""

import sys
import json
import time
import urllib.request
import hashlib

import logging
import os

try:
    import boto3
    from botocore.exceptions import ClientError
    from botocore.config import Config
except ImportError:
    print("boto3 not installed. Run:")
    print("  pip3 install --break-system-packages boto3")
    sys.exit(1)


API = "http://localhost:8080/api"


def http_post(path, data=None, headers=None):
    req = urllib.request.Request(API + path, data=data, headers=headers or {}, method="POST")
    if data:
        req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req) as r:
        return json.loads(r.read())


# 1. Get JWT, then S3 credentials
print("== 1. Create S3 credentials =================================")
jwt_resp = http_post("/auth/login",
    json.dumps({"email": "admin@local", "password": "admin"}).encode())
jwt = jwt_resp["token"]

creds = http_post("/auth/s3-credentials",
    json.dumps({"name": "boto-test"}).encode(),
    {"Authorization": f"Bearer {jwt}"})

print(f"   AKID:   {creds['accessKeyId']}")
print(f"   secret: {creds['secretAccessKey'][:10]}...")

# 2. boto3 client
s3 = boto3.client(
    "s3",
    endpoint_url=API,
    aws_access_key_id=creds["accessKeyId"],
    aws_secret_access_key=creds["secretAccessKey"],
    region_name="us-east-1",
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)

# Enable debug logging if DEBUG=1 env var set
if os.getenv("DEBUG"):
    boto3.set_stream_logger("botocore", logging.DEBUG)

# 3. List buckets
print("\n== 2. list_buckets ===========================================")
try:
    r = s3.list_buckets()
    print(f"   Owner: {r['Owner']}")
    print(f"   Buckets: {[b['Name'] for b in r.get('Buckets', [])]}")
    print("   OK")
except ClientError as e:
    print(f"   FAIL: {e.response['Error']}")
    sys.exit(1)

bucket = f"boto-{int(time.time())}"

# 4. Create bucket
print(f"\n== 3. create_bucket {bucket} ==============================")
try:
    s3.create_bucket(Bucket=bucket)
    print("   OK")
except ClientError as e:
    print(f"   FAIL: {e.response['Error']}")
    sys.exit(1)

# 5. Upload a small file
print("\n== 4. put_object (small) =====================================")
body = b"hello from boto3 via SigV4"
try:
    r = s3.put_object(Bucket=bucket, Key="hello.txt", Body=body,
                      ContentType="text/plain")
    print(f"   ETag: {r['ETag']}")
    print("   OK")
except ClientError as e:
    print(f"   FAIL: {e.response['Error']}")
    sys.exit(1)

# 6. List objects
print("\n== 5. list_objects_v2 ========================================")
try:
    r = s3.list_objects_v2(Bucket=bucket)
    keys = [o["Key"] for o in r.get("Contents", [])]
    print(f"   keys: {keys}")
    assert "hello.txt" in keys
    print("   OK")
except ClientError as e:
    print(f"   FAIL: {e.response['Error']}")
    sys.exit(1)

# 7. Download + verify
print("\n== 6. get_object =============================================")
try:
    r = s3.get_object(Bucket=bucket, Key="hello.txt")
    data = r["Body"].read()
    assert data == body, f"mismatch: got {data!r}"
    print(f"   data matches ({len(data)} bytes)")
    print("   OK")
except ClientError as e:
    print(f"   FAIL: {e.response['Error']}")
    sys.exit(1)

# 8. Multipart upload via upload_file (boto3 picks multipart automatically for > 8 MiB)
print("\n== 7. upload_file (multipart, 20 MiB) ========================")
import os, tempfile
big = tempfile.NamedTemporaryFile(delete=False)
big.write(os.urandom(20 * 1024 * 1024))
big.close()
orig_md5 = hashlib.md5(open(big.name, "rb").read()).hexdigest()
try:
    from boto3.s3.transfer import TransferConfig
    config = TransferConfig(
        multipart_threshold=8 * 1024 * 1024,
        max_concurrency=4,
        multipart_chunksize=5 * 1024 * 1024,
    )
    s3.upload_file(big.name, bucket, "big.bin", Config=config)
    print(f"   uploaded 20 MiB, orig md5 {orig_md5}")
    print("   OK")
except ClientError as e:
    print(f"   FAIL: {e.response['Error']}")
    sys.exit(1)
finally:
    os.unlink(big.name)

# 9. Cleanup
print("\n== 8. cleanup ================================================")
s3.delete_object(Bucket=bucket, Key="hello.txt")
s3.delete_object(Bucket=bucket, Key="big.bin")
s3.delete_bucket(Bucket=bucket)
print("   OK")

# Revoke credential
req = urllib.request.Request(
    API + f"/auth/s3-credentials/{creds['accessKeyId']}",
    headers={"Authorization": f"Bearer {jwt}"},
    method="DELETE",
)
urllib.request.urlopen(req).read()

print("\nAll Part 10 (boto3) tests passed.")
