# API Reference

Every endpoint, what it takes, what it returns.

All authenticated endpoints accept either `Authorization: Bearer ...`
(JWT or API key) or `Authorization: AWS4-HMAC-SHA256 ...` (SigV4).

Base URL: `http://localhost:8080/api` (or your tunnel hostname + `/api`).

## Error envelope

Every JSON error response uses one shape:

```json
{
  "code":    "QUOTA_EXCEEDED",
  "message": "human readable",
  "details": {                          // optional, machine-readable
    "requestedBytes": 5242880,
    "usedBytes":      2014871552,
    "quotaBytes":     2147483648,
    "availableBytes": 132612096,
    "deficitBytes":   0
  }
}
```

`details` is populated for:
- `QUOTA_EXCEEDED` on `PUT /:bucket/:key` and on multipart `UploadPart` —
  fields shown above.
- Future capacity / size errors will follow the same shape.

S3-SDK clients get standard XML error responses instead (negotiated via
`User-Agent` / `Content-Type`).

## Transcode status states

Asynchronous transcode outcomes show up in `objects.transcodeStatus`
(returned by `GET /:bucket/:key?info`). See
[architecture/database-schema.md#transcode-status-state-machine](./architecture/database-schema.md#transcode-status-state-machine)
for the full table; the quota-related ones are:

| Status | Meaning | `objects.transcoded` body |
|---|---|---|
| `skipped_quota` | Pre-flight estimate exceeded quota; no jobs ever ran | `{"quota":{"estimatedBytes":..,"availableBytes":..,"deficitBytes":..,"reason":"skipped_quota"}}` |
| `failed_quota`  | Output exceeded reservation at publish; segments reaped | `{"quota":{"actualBytes":..,"reservedBytes":..,"deficitBytes":..,"reason":"failed_quota"}}` |

Originals are always playable; only the HLS ladder is missing. Re-trigger
with `DELETE /:bucket/:key?transcodes` after freeing space.

---

## Public (no auth)

### `GET /healthz`
Liveness check. Pings the database.

| | |
|---|---|
| Returns | `200 "ok"` or `503 "db down"` |

### `POST /auth/login`
Exchange email+password for a 24h JWT.

```json
{ "email": "you@example.com", "password": "..." }
```

| Response | Body |
|---|---|
| `200` | `{ "token": "eyJ...", "user": { id, email, name, role, quotaBytes, usedBytes } }` |
| `401` | `{ "code": "BAD_CREDS", "message": "invalid credentials" }` |

---

## Auth — user self-service

### `GET /auth/me`
Current authenticated user.

```json
{ "id": "...", "email": "...", "name": "...", "role": "user|admin",
  "quotaBytes": 2147483648, "usedBytes": 1402814464,
  "totpEnabled": false }
```

`usedBytes` is re-read from the DB on every call so dashboards see live
transcode publishes without waiting for the JWT to refresh.

### `GET /auth/storage`

Per-bucket + trash + versions + reserved-transcode breakdown of
`used_bytes`. Powers the dashboard's stacked storage chart.

```json
{
  "quotaBytes": 2147483648,
  "usedBytes":  1402814464,
  "trashBytes":   98765432,
  "versionsBytes": 0,
  "reservedBytes": 0,
  "buckets": [
    {
      "id": "...", "name": "photos", "archived": false,
      "originalBytes":   524288000,
      "transcodedBytes":         0,
      "reservedBytes":           0,
      "trashBytes":      98765432,
      "totalBytes":     623053432
    },
    {
      "id": "...", "name": "videos", "archived": false,
      "originalBytes":   177155061,
      "transcodedBytes": 602831871,
      "reservedBytes":           0,
      "trashBytes":              0,
      "totalBytes":     779986932
    }
  ]
}
```

`usedBytes` is the authoritative atomic counter from `users.used_bytes`.
The breakdown sum can lag by milliseconds during in-flight transactions;
if it diverges by more than ~1 MB the dashboard surfaces a drift warning
pointing at `scripts/quota-reconcile.sql`.

### `GET /auth/keys`
List your Bearer API keys (no secrets — just `prefix` for identification).

### `POST /auth/keys`
Create a new Bearer API key.

Body: `{ "name": "my-laptop", "expiresAt": "..." (optional ISO date) }`

Response includes the **plaintext key** in `key` — shown ONCE, never again.

### `DELETE /auth/keys/{id}`
Revoke a key.

### `GET /auth/s3-credentials`
List your AWS-style credentials (no secrets — just `accessKeyId`).

### `POST /auth/s3-credentials`
Create AKID + secret. Response includes plaintext secret in
`secretAccessKey` — shown ONCE.

### `DELETE /auth/s3-credentials/{accessKeyId}`
Revoke a credential.

---

## Buckets

### `GET /`
List your buckets.

Response (JSON):
```json
{ "buckets": [ { "id": "...", "name": "photos", "createdAt": "..." } ] }
```

Response (XML, for SigV4 clients): standard S3 `ListAllMyBucketsResult`.

### `PUT /{bucket}`
Create a bucket. Name rules: 3-63 chars, lowercase alphanum + dots/hyphens,
can't start or end with non-alphanum, no reserved names (`admin`, `auth`,
`healthz`, `stream`, `static`, `public`).

| Response | |
|---|---|
| `200` | Created (S3 clients: empty body; dashboard: JSON bucket) |
| `400 INVALID_NAME` / `RESERVED_NAME` | bad name |
| `409 BUCKET_EXISTS` | you already own this name |

### `HEAD /{bucket}`
| Response | |
|---|---|
| `200` | exists |
| `404` | doesn't exist or not yours |

### `DELETE /{bucket}`
Bucket must be empty.

| Response | |
|---|---|
| `204` | deleted |
| `404 NO_SUCH_BUCKET` | not found |
| `409 BUCKET_NOT_EMPTY` | delete the objects first |

---

## Objects

### `GET /{bucket}?prefix=...&max-keys=...`
List objects. Query params:

| Param | Default | Meaning |
|---|---|---|
| `prefix` | empty | only return keys starting with this |
| `max-keys` | 1000 | max items (capped at 1000) |

Returns JSON for native clients, XML `ListBucketResult` for SigV4 clients.

### `PUT /{bucket}/{key}`
Upload an object. Body is the file contents (streamed, not buffered).

Headers:
- `Content-Type: ...` (default `application/octet-stream`)
- `Content-Length: N` (recommended for accurate quota pre-check)

| Response | |
|---|---|
| `200 OK + ETag header` | uploaded |
| `400 INVALID_KEY` | empty key |
| `404 NO_SUCH_BUCKET` | bucket doesn't exist |
| `507 QUOTA_EXCEEDED` | user over quota |
| `507 DISK_FULL` | physical disk past `disk_full_threshold_pct` |

### `GET /{bucket}/{key}`
Download an object. Supports HTTP Range requests for partial / seekable
downloads (used by video players).

| Response | |
|---|---|
| `200` / `206` | with body |
| `404 NO_SUCH_KEY` | not found |

### `HEAD /{bucket}/{key}`
Metadata only (Content-Length, ETag, Last-Modified, Content-Type).

### `GET /{bucket}/{key}?info`
**Extended metadata** (PersonalS3-specific, not in S3 spec). Returns
the object UUID + transcoded paths — needed by the dashboard to build
HLS stream URLs.

```json
{
  "objectId": "uuid",
  "bucket": "photos", "key": "cat.jpg",
  "size": 12345, "etag": "abc...", "contentType": "image/jpeg",
  "lastModified": "...",
  "transcoded": { "type": "image", "webp": "original.webp", "avif": "...", "thumbnails": [...] },
  "transcodeStatus": "done"
}
```

### `DELETE /{bucket}/{key}`
Delete an object. Refunds quota.

---

## Multipart upload

These share `/{bucket}/{key}` with single-part operations — query strings select.

### `POST /{bucket}/{key}?uploads`
Initiate. Returns `{ uploadId: "..." }`.

### `PUT /{bucket}/{key}?partNumber=N&uploadId=X`
Upload one part (`N` = 1..10000). Body = part data. Returns `ETag` header.

### `POST /{bucket}/{key}?uploadId=X`
Complete. Body is JSON or XML with the part list:

JSON (native clients):
```json
{ "parts": [ { "partNumber": 1, "etag": "abc..." }, ... ] }
```

XML (S3 clients): standard `CompleteMultipartUpload`.

All parts except the last must be ≥ 5 MiB. Returns final assembled
object's ETag (`md5-of-md5s-N` format).

### `DELETE /{bucket}/{key}?uploadId=X`
Abort. Refunds reserved quota; deletes uploaded parts from disk.

### `GET /{bucket}/{key}?uploadId=X`
List uploaded parts. Used for resumable uploads.

---

## Admin (role=admin only)

### `GET /admin/users`
List all users + their quotas + usage.

### `POST /admin/users`
Create a user.

```json
{
  "email": "alice@example.com",
  "name": "Alice",
  "password": "temporary",
  "role": "user",                  // or "admin"
  "quotaBytes": 53687091200        // 50 GB; 0 = default
}
```

| Response | |
|---|---|
| `201` | user created |
| `409 EMAIL_EXISTS` | email taken |
| `409 WOULD_OVERCOMMIT` | granting this quota would push total above system cap |

### `PATCH /admin/users/{id}`
Edit user. All fields optional:

```json
{ "quotaBytes": 107374182400, "role": "admin", "isActive": false, "name": "..." }
```

### `DELETE /admin/users/{id}`
Soft delete (sets `is_active = false`). User's data persists.

### `GET /admin/audit?user=&action=&since=&until=&limit=`
Audit log. Filters all optional:

| Param | Meaning |
|---|---|
| `user` | substring of email |
| `action` | exact match (e.g. `PUT_OBJECT`) |
| `since` | RFC 3339 timestamp |
| `until` | RFC 3339 timestamp |
| `limit` | max rows (default 100, cap 1000) |

### `GET /admin/stats`
System-wide statistics + physical disk info:

```json
{
  "userCount": 5,
  "bucketCount": 12,
  "objectCount": 8423,
  "totalUsedBytes": 234234234,
  "totalQuotaBytes": 539999999999,
  "transcodeJobs": { "pending": 0, "processing": 2, "done": 1284, "failed": 0 },
  "requestsLastHour": 412,
  "topActions": [ { "action": "GET_OBJECT", "count": 200 }, ... ],
  "disk": {
    "path": "/storage",
    "physicalTotal": 1099511627776,
    "physicalUsed": 234234234234,
    "physicalFree": 865277393542,
    "reservedBytes": 5368709120,
    "effectiveTotal": 1094142918656,
    "allocatedToUsers": 539999999999,
    "actuallyUsed": 234234234,
    "overcommitAllowed": false,
    "diskFullPct": 95,
    "overcommitted": false
  }
}
```

### `GET /admin/system-config`
List all runtime config keys.

### `PATCH /admin/system-config/{key}`
Update one config value.

```json
{ "value": "10737418240" }
```

Valid keys: `total_quota_bytes`, `reserved_bytes`, `disk_full_threshold_pct`,
`overcommit_allowed`.

---

## HTTP status code reference

| Code | When |
|---|---|
| `200` | OK with body |
| `201` | Created (new resource) |
| `204` | OK no body |
| `400` | Malformed request |
| `401` | Missing or invalid credentials |
| `403` | Authenticated but not authorized (admin-only) |
| `404` | Not found / no access |
| `409` | Conflict (e.g. already exists, would overcommit) |
| `429` | Rate limit exceeded; check `Retry-After` header |
| `500` | Server bug — check API logs |
| `503` | Service unavailable (DB down) |
| `507` | Insufficient storage (quota exceeded or disk full) |

## Error response format

JSON (native clients):
```json
{ "code": "QUOTA_EXCEEDED", "message": "..." }
```

XML (S3 clients): standard `<Error><Code>...</Code>...</Error>`.

## Rate limiting

- 1000 req/min per user (10000 for admins)
- Counted across ALL endpoints
- Response headers on every request:
  ```
  X-RateLimit-Limit:     1000
  X-RateLimit-Remaining: 994
  ```
- Exceeded → `429 Too Many Requests` + `Retry-After: 60`.
