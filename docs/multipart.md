# Multipart upload

This page documents PersonalS3's multipart upload protocol, where it
diverges from AWS S3, and how to drive it from every supported client.

If you've used AWS multipart before, the **TL;DR** is: same four
endpoints, same query-string shape, same XML on the wire for SDK
clients, **plus** a JSON request/response variant for native callers
(dashboard, custom scripts, the `ps3` CLI's library code).

---

## Why multipart?

A single PUT works fine for small files. For anything large enough that
a network blip would force you to restart from scratch, you want to:

- **Parallelize** — saturate the link with N concurrent uploads instead
  of one.
- **Resume** — a failed part is the only thing that needs to retransmit.
- **Stream** — never load the whole file into memory.
- **Verify atomically** — the final object only appears after every
  part is checked, so half-uploaded files never become visible.

Multipart is the protocol for all four.

---

## Lifecycle (the four endpoints)

```
1. Initiate  ──►  server returns uploadId
2. UploadPart × N (parallel, any order)
3. Complete  ──►  server assembles, returns final ETag
   (or)
3'. Abort    ──►  server discards everything, refunds quota
```

The path is the same as a normal `PUT /:bucket/:key`. The query string
selects multipart-vs-single-part operation.

### 1. Initiate — `POST /:bucket/:key?uploads`

Begin an upload. Body is ignored.

**Returns** an `uploadId` (UUID). Save it — every subsequent call for
this object needs it.

```json
// JSON (default for native clients)
{ "bucket": "videos", "key": "movie.mp4", "uploadId": "abc-123..." }
```

```xml
<!-- XML (returned when the request looks like an S3 SDK call) -->
<InitiateMultipartUploadResult>
  <Bucket>videos</Bucket>
  <Key>movie.mp4</Key>
  <UploadId>abc-123...</UploadId>
</InitiateMultipartUploadResult>
```

Server-side this writes one row to `multipart_uploads` and creates an
empty `buckets/{B}/tmp/{uploadId}/` directory on disk.

### 2. UploadPart — `PUT /:bucket/:key?partNumber=N&uploadId=X`

Send one part's bytes as the request body.

- `partNumber` must be in `[1, 10000]`.
- Set `Content-Length` to the part size (lets the server pre-reserve
  quota and reject early if you'd blow your cap).
- Parts can be uploaded in **any order** and in **parallel**.
- Re-uploading the same `partNumber` overwrites the previous attempt
  (useful for retries — the ETag and stored bytes are replaced
  atomically).

**Returns** an MD5 hex digest in the `ETag` response header. Capture
that — Complete will reject the upload if it doesn't match.

```
HTTP/1.1 200 OK
ETag: "9c4f9b1d..."
```

### 3. Complete — `POST /:bucket/:key?uploadId=X`

Tell the server "all my parts are uploaded, assemble them in this
order." Body is the part list — either JSON or XML.

```json
// JSON
{
  "parts": [
    { "partNumber": 1, "etag": "9c4f9b1d..." },
    { "partNumber": 2, "etag": "7a2e1c4f..." },
    ...
  ]
}
```

```xml
<!-- XML (S3 SDKs send this) -->
<CompleteMultipartUpload>
  <Part><PartNumber>1</PartNumber><ETag>"9c4f9b1d..."</ETag></Part>
  <Part><PartNumber>2</PartNumber><ETag>"7a2e1c4f..."</ETag></Part>
</CompleteMultipartUpload>
```

The server:
1. Looks up every part's stored ETag and size.
2. Rejects on **missing part**, **ETag mismatch**, or **part below
   5 MiB** (last part is exempt — same rule as S3).
3. Concatenates parts into `objects/{H}/data.assembling`.
4. Atomically renames into place.
5. Inserts the `objects` row.
6. Enqueues any applicable transcode jobs (video → HLS, image → MIPs).
7. Deletes the `tmp/{uploadId}/` directory.

**Returns** the final ETag, formatted exactly like S3:
`md5(concat(md5_bytes_of_each_part)) + "-" + part_count`.

```json
{ "bucket": "videos", "key": "movie.mp4",
  "etag": "f7d4...e1-20", "size": 104857600 }
```

### 3'. Abort — `DELETE /:bucket/:key?uploadId=X`

Cancel everything. Refunds all reserved quota, deletes parts on disk,
marks the row `aborted`. Idempotent.

### Listing parts — `GET /:bucket/:key?uploadId=X`

Useful for **resumable uploads**: if the client crashes, query this
endpoint to find out which parts the server already has, then only
re-upload the missing ones.

```json
{
  "uploadId": "abc-123...",
  "parts": [
    { "partNumber": 1, "etag": "9c4...", "size": 5242880 },
    { "partNumber": 3, "etag": "8a1...", "size": 5242880 }
    // part 2 missing — retry it
  ]
}
```

---

## PersonalS3 vs AWS S3 — concrete differences

| Topic | AWS S3 | PersonalS3 |
|---|---|---|
| **Endpoints** | `POST ?uploads`, `PUT ?partNumber=&uploadId=`, `POST ?uploadId=`, `DELETE ?uploadId=`, `GET ?uploadId=` | **Identical** — drop-in. |
| **Part size minimum** | 5 MiB (all but last) | 5 MiB (all but last) — same. |
| **Part size maximum** | 5 GiB | No hard cap; bounded by request body and disk. |
| **Object size maximum** | 5 TiB | Bounded by your disk and quota. |
| **Max parts per upload** | 10,000 | 10,000. |
| **In-progress retention** | Lives until aborted; bills you for storage. | **Auto-expires after 7 days**, then reaped by the cleaner. No silent storage bill. |
| **Wire format on Initiate/Complete** | XML only. | XML for S3 SDKs; **JSON** for everyone else (auto-detected by `User-Agent` / `Content-Type`). |
| **Final ETag** | `md5(concat(md5s)) + "-N"` | Same exact formula — interop-safe. |
| **Per-part ETag** | MD5 of part bytes (hex) | MD5 of part bytes (hex) — same. |
| **Quota reservation** | None — S3 lets you upload until billing reacts. | **Pre-reserved per part** against your quota. UploadPart returns `507 Insufficient Storage` (`QUOTA_EXCEEDED`) as soon as it would exceed, with a `details` block listing `{requestedBytes, availableBytes, deficitBytes, ...}` so clients can show "you need N more bytes". Refunded on Abort or part re-upload. |
| **Disk-health gate** | N/A | Each part PUT checks `CheckDiskHealthy`; returns `507 DISK_FULL` if the host volume is past `DISK_HARD_PCT`. |
| **Authentication** | AWS SigV4. | Bearer JWT (default), API-key bearer, or AWS SigV4 — your client picks. SigV4 lets boto3/AWS CLI/rclone work unchanged. |
| **Bucket lifecycle "expire incomplete multipart" rule** | A configurable LCR. | Fixed 7-day expiry on the `multipart_uploads` row; the cleaner deletes the row + on-disk parts when it fires. |
| **Re-upload same part** | New ETag silently replaces old. | Same — `ON CONFLICT (upload_id, part_number) DO UPDATE`. Quota delta reconciled. |
| **List in-progress uploads cross-key** | `ListMultipartUploads` endpoint. | **Not implemented.** List a specific upload's parts only. Track upload IDs client-side. |
| **Tagging on Complete (`x-amz-tagging`)** | Supported. | Ignored. |
| **SSE / server-side encryption** | Supported. | Not implemented — disk-at-rest encryption is the operator's job (LUKS, encrypted ZFS, etc.). |

---

## How each client does multipart

### `ps3` CLI

The CLI streams the file as one PUT — no multipart split. Resumability
isn't there yet on the CLI side. For multi-GB files where a network
blip would be painful, **prefer the dashboard** (which does split) or
**the AWS CLI** (which does split, and talks to PersonalS3 via SigV4).
Tracked as a future CLI feature.

### Dashboard (browser)

Built-in. See `dashboard/lib/multipart.ts`:

- **Threshold**: files `> 8 MiB` are uploaded multipart, smaller ones
  go through a single `PUT`.
- **Part size**: `5 MiB` (the minimum).
- **Concurrency**: `4` parts in flight at once.
- **Resumability**: not currently — a closed tab leaves the upload to
  expire after 7 days (cleaner refunds the quota).

Just drag-drop into the file browser. The UI shows per-part progress
under "Active operations."

### AWS CLI

Works out of the box. Use the bucket browser endpoint as your S3
endpoint:

```bash
aws configure set aws_access_key_id YOUR_ACCESS_KEY
aws configure set aws_secret_access_key YOUR_SECRET_KEY
aws configure set default.region us-east-1

aws --endpoint-url http://localhost:8080 s3 cp ./big-file.mp4 s3://videos/
```

The AWS CLI auto-splits files over 8 MiB into 8 MiB parts with up to
10 concurrent transfers. Override via `~/.aws/config`:

```ini
[default]
s3 =
  multipart_threshold = 64MB
  multipart_chunksize = 16MB
  max_concurrent_requests = 8
```

Credentials are issued under **Settings → API Keys** in the dashboard.

### boto3 (Python)

Same — use the high-level `upload_file`:

```python
import boto3

s3 = boto3.client(
    "s3",
    endpoint_url="http://localhost:8080",
    aws_access_key_id="...", aws_secret_access_key="...",
    region_name="us-east-1",
)

from boto3.s3.transfer import TransferConfig
cfg = TransferConfig(multipart_threshold=16 * 1024 * 1024,  # 16 MiB
                     multipart_chunksize=8 * 1024 * 1024,
                     max_concurrency=8,
                     use_threads=True)
s3.upload_file("./big-file.mp4", "videos", "big-file.mp4", Config=cfg)
```

### rclone

```bash
# one-time
rclone config
# pick s3 → provider: Other → endpoint: http://localhost:8080
# access_key_id / secret_access_key from dashboard

rclone copy ./folder ps3:videos/ \
  --s3-chunk-size 16M \
  --s3-upload-concurrency 8
```

### curl / shell script (raw protocol)

Useful for embedded systems, CI, or anywhere you don't want an SDK.

```bash
TOKEN="eyJhbGc..."         # JWT from /api/login, or paste an API key
HOST="http://localhost:8080"
BUCKET="videos"
KEY="movie.mp4"
FILE="./movie.mp4"
PART_SIZE=$((5 * 1024 * 1024))

# 1. Initiate
UPLOAD_ID=$(curl -fsS -X POST \
  -H "Authorization: Bearer $TOKEN" \
  "$HOST/$BUCKET/$KEY?uploads" | jq -r .uploadId)
echo "uploadId=$UPLOAD_ID"

# 2. Split + upload parts
SIZE=$(stat -c%s "$FILE")
NPARTS=$(( (SIZE + PART_SIZE - 1) / PART_SIZE ))
PARTS_JSON='['
for ((n=1; n<=NPARTS; n++)); do
  OFFSET=$(( (n - 1) * PART_SIZE ))
  ETAG=$(dd if="$FILE" bs=$PART_SIZE skip=$((n-1)) count=1 2>/dev/null |
    curl -fsS -X PUT --data-binary @- -H "Authorization: Bearer $TOKEN" \
      -D - "$HOST/$BUCKET/$KEY?partNumber=$n&uploadId=$UPLOAD_ID" |
    grep -i '^ETag:' | sed -E 's/.*"([^"]+)".*/\1/')
  echo "part $n etag=$ETAG"
  PARTS_JSON+="{\"partNumber\":$n,\"etag\":\"$ETAG\"}"
  (( n < NPARTS )) && PARTS_JSON+=','
done
PARTS_JSON+=']'

# 3. Complete
curl -fsS -X POST -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  --data "{\"parts\":$PARTS_JSON}" \
  "$HOST/$BUCKET/$KEY?uploadId=$UPLOAD_ID"
```

To abort instead: `curl -X DELETE "$HOST/$BUCKET/$KEY?uploadId=$UPLOAD_ID"`.

---

## Quota & failure semantics

This is the part most people care about and most other write-ups skip.

**Reservation timeline** for one part:

```
PUT ?partNumber=N&uploadId=X    Content-Length: 5242880
│
├─ pre-reserve QuotaReserve(+5242880)
│  └─ if would exceed quota → 507 QUOTA_EXCEEDED, nothing written
│
├─ stream to objects/{B}/tmp/{X}/part-NNNN.tmp + md5 in one pass
│  └─ on io.Copy error → tmp removed, full reservation refunded
│
├─ rename .tmp → final
│  └─ on rename error → final removed, reservation refunded
│
├─ reconcile: adjustment = (actual_n - oldSize) - reserved
│  (handles re-upload of same partNumber where oldSize > 0,
│   and Content-Length / actual-bytes mismatch)
│
└─ UPSERT multipart_parts + update total_size
```

**On Complete**:
- All part bytes are already charged to `used_bytes` via the per-part
  reservations.
- If `Complete` overwrites an existing object at the same key, the
  **previous object's size is refunded** before the rename.
- The tmp dir is deleted but its bytes have already been concatenated
  into the final object — no double-charge.

**On Abort**:
- `multipart_uploads.total_size` is refunded in one shot.
- Tmp dir + parts rows deleted.

**On client disappearance (no Abort, no Complete)**:
- The `multipart_uploads` row sits with `status='in-progress'` and an
  `expires_at` of `now() + 7 days`.
- The cleaner's `multipart_sweep` runs every tick. Once
  `expires_at < now()`, it sets `status='aborted'`, refunds quota, and
  removes the tmp dir.
- Result: quota leaks self-heal within hours of expiry.

**Drift safety net**: `scripts/quota-reconcile.sql` recomputes
`users.used_bytes` from `SUM(objects.size_bytes + transcoded_bytes) +
SUM(object_versions.size_bytes)` per owner. Idempotent; recommended
weekly via cron (see `production-tuning.md`). Run this if you ever
suspect your `used_bytes` is wrong.

---

## When NOT to use multipart

- **Files under ~8 MiB**: not worth the round-trip overhead. Use a
  single `PUT /:bucket/:key`. The dashboard and AWS SDKs already do
  this automatically based on threshold.
- **Pre-signed PUT URLs**: presign issues a single URL good for one
  `PUT` of the whole body. Multipart isn't presign-compatible — for
  large pre-signed uploads, use the dashboard's signed-pull pattern
  instead, or do the multipart through a JWT-authenticated session.

---

## See also

- `api-reference.md` — endpoint reference (skim if you just want shapes)
- `architecture/data-flows.md#flow-3-upload-a-large-file-multipart`
- `storage-management.md` — quota mechanics
- `troubleshooting.md` — "my quota is wrong" recipes
