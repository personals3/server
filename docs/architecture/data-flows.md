# Data Flows

What actually happens, second-by-second, for each common operation.

---

## Flow 1: User logs into the dashboard

```
Browser                    nginx              dashboard           api               postgres
   │                         │                  │                  │                   │
   │ GET /                   │                  │                  │                   │
   ├────────────────────────►│                  │                  │                   │
   │                         │ proxy_pass →     │                  │                   │
   │                         ├─────────────────►│                  │                   │
   │                         │                  │ render login.tsx │                   │
   │                         │◄─────────────────┤ HTML             │                   │
   │ <html...>               │                  │                  │                   │
   │◄────────────────────────┤                  │                  │                   │
   │                         │                  │                  │                   │
   │   user types creds & submits                                                      │
   │                         │                                                         │
   │ POST /api/auth/login    │                  │                  │                   │
   │ {email,password}        │                  │                  │                   │
   ├────────────────────────►│                  │                  │                   │
   │                         │ strip /api,      │                  │                   │
   │                         │ proxy_pass →     │                  │                   │
   │                         ├─────────────────────────────────────►│                   │
   │                         │                  │                  │ SELECT password_  │
   │                         │                  │                  │   hash WHERE     │
   │                         │                  │                  │   email=?         │
   │                         │                  │                  ├──────────────────►│
   │                         │                  │                  │◄──────────────────┤
   │                         │                  │                  │ bcrypt.compare    │
   │                         │                  │                  │ → IssueJWT(HS256) │
   │                         │                  │                  │ → {token, user}   │
   │                         │◄─────────────────────────────────────┤                   │
   │ 200 {"token":"eyJ..."}  │                                                         │
   │◄────────────────────────┤                                                         │
   │                         │                                                         │
   │   localStorage.setItem("ps3_token", token)                                        │
   │   router.replace("/dashboard")                                                    │
   │                                                                                   │
   │   subsequent requests include Authorization: Bearer eyJ...                        │
```

---

## Flow 2: Upload a small file (no multipart)

```
Browser                      nginx              api                fs                 postgres
   │                           │                  │                  │                    │
   │  drag-drop cat.jpg (3MB)                                                             │
   │                           │                  │                  │                    │
   │ PUT /api/photos/cat.jpg   │                  │                  │                    │
   │ Authorization: Bearer JWT │                  │                  │                    │
   │ Content-Type: image/jpeg  │                  │                  │                    │
   │ Body: <3MB bytes>         │                  │                  │                    │
   ├──────────────────────────►│                  │                  │                    │
   │                           │ strip /api,      │                  │                    │
   │                           │ stream body →    │                  │                    │
   │                           ├─────────────────►│                  │                    │
   │                           │                  │ Authenticator    │                    │
   │                           │                  │   verify JWT     ├───────────────────►│
   │                           │                  │   load *User     │  SELECT FROM users │
   │                           │                  │◄─────────────────│◄───────────────────┤
   │                           │                  │ CheckDiskHealthy │                    │
   │                           │                  │   statfs(/storage)                    │
   │                           │                  │ QuotaReserve(+3MB)                    │
   │                           │                  ├───────────────────────────────────────►│
   │                           │                  │  UPDATE users SET used_bytes += 3MB   │
   │                           │                  │  WHERE used + 3MB <= quota            │
   │                           │                  │◄───────────────────────────────────────┤
   │                           │                  │ fs.WriteObject:                       │
   │                           │                  │   open data.tmp                       │
   │                           │                  │   io.Copy(tmp, body)  ←── streaming   │
   │                           │                  │   while computing MD5                 │
   │                           │                  │   rename data.tmp → data              │
   │                           │                  ├───────────────────────────────────────►│
   │                           │                  │  INSERT INTO objects RETURNING id     │
   │                           │                  │◄───────────────────────────────────────┤
   │                           │                  │ enqueueTranscode():                   │
   │                           │                  │   if image/video/audio:               │
   │                           │                  ├───────────────────────────────────────►│
   │                           │                  │  INSERT INTO transcode_jobs           │
   │                           │                  │  UPDATE objects SET transcode_status  │
   │                           │                  │    = 'pending'                        │
   │                           │                  │◄───────────────────────────────────────┤
   │                           │ 200 OK           │                                       │
   │                           │ ETag: "abc..."   │                                       │
   │                           │◄─────────────────┤                                       │
   │ 200 OK ETag: "abc..."     │                                                         │
   │◄──────────────────────────┤                                                         │
   │                           │                                                         │
   │  (async, in another container — see Flow 4)                                         │
```

---

## Flow 3: Upload a large file (multipart, 100 MB → 20 parts of 5 MB)

```
Browser (uploads in 4 parallel workers)              api                fs
   │                                                  │                  │
   │ POST /api/videos/movie.mp4?uploads               │                  │
   ├─────────────────────────────────────────────────►│                  │
   │                                                  │ INSERT multipart_uploads
   │                                                  │ mkdir tmp/{upload-id}/
   │ 200 {"uploadId": "abc-123"}                      │                  │
   │◄─────────────────────────────────────────────────┤                  │
   │                                                                     │
   │ ── pool of 4 parallel uploads ──                                    │
   │                                                                     │
   │ PUT ...?partNumber=1&uploadId=abc-123 (5MB) ────►│                  │
   │ PUT ...?partNumber=2&uploadId=abc-123 (5MB) ────►│ for each:        │
   │ PUT ...?partNumber=3&uploadId=abc-123 (5MB) ────►│  CheckDiskHealthy│
   │ PUT ...?partNumber=4&uploadId=abc-123 (5MB) ────►│  QuotaReserve(+5MB)
   │                                                  │  write part-NNNN │
   │                                                  │  UPSERT multipart_parts
   │                                                  │  total_size += n │
   │ ◄── 4× {ETag: "..."}                             │                  │
   │                                                                     │
   │ (keep going until all 20 parts done)                                │
   │                                                                     │
   │ POST /api/videos/movie.mp4?uploadId=abc-123                         │
   │ {"parts": [{partNumber:1,etag:".."}, ...]}      │                  │
   ├─────────────────────────────────────────────────►│                  │
   │                                                  │ Verify all parts │
   │                                                  │   exist & ETags  │
   │                                                  │   match          │
   │                                                  │ Verify all parts │
   │                                                  │   except last ≥5MB
   │                                                  │ Concatenate:     │
   │                                                  │   cat part-* >   │
   │                                                  │   objects/{hash}/data
   │                                                  │ Compute final ETag
   │                                                  │   md5(md5(p1)+   │
   │                                                  │     md5(p2)+...)│
   │                                                  │   + "-N"         │
   │                                                  │ INSERT objects   │
   │                                                  │   RETURNING id   │
   │                                                  │ enqueueTranscode │
   │                                                  │ DELETE tmp dir   │
   │                                                  │ UPDATE multipart_│
   │                                                  │   uploads SET    │
   │                                                  │   status=complete│
   │ 200 {"etag": "abc...-20", "size": 104857600}     │                  │
   │◄─────────────────────────────────────────────────┤                  │
```

If the browser aborts (closes tab, refreshes) mid-upload, an `Abort`
endpoint can be called to clean up — otherwise the in-progress upload
sits in the DB until its 7-day expiry. Quota is refunded on abort.

---

## Flow 4: Background transcoding of an uploaded video

The pipeline fans out into per-rung jobs that run in parallel across the
worker thread pool. A `video_finalize` job waits on its siblings and
publishes the final `objects.transcoded` row.

```
api EnqueueTranscode                                       worker pool       ffmpeg
       │                                                       │                │
       │ ffprobe (host) → height, duration                     │                │
       │ ladder = PickLadder(height)                           │                │
       │ estimate = Σ(rung.VBR+rung.ABR) × duration × 1.4      │                │
       │                                                       │                │
       │  ATOMIC QuotaReserve(+estimate)                       │                │
       │   ├── insufficient → status='skipped_quota'           │                │
       │   │                  no jobs enqueued (Flow 4a)       │                │
       │   └── success → UPDATE transcode_reserved_bytes = estimate
       │                                                       │                │
       │ INSERT transcode_jobs:                                │                │
       │   N × video_quality_{H}p   (pending, group_id=G)      │                │
       │   1 × video_thumbnails     (pending, group_id=G)      │                │
       │   1 × video_finalize       (waiting, group_id=G)      │                │
       │                                                       │                │
       │              (workers poll postgres every 2s,         │                │
       │               FOR UPDATE SKIP LOCKED on `pending`)    │                │
       │                                                       ├───────────────►│
       │                                                       │  per-rung      │
       │                                                       │  GPU/VAAPI     │
       │                                                       │  encode →      │
       │                                                       │  /storage/segments/{O}/{H}p/
       │                                                       │◄───────────────┤
       │                                                       │                │
       │ (siblings done → _promote_waiting → finalize 'pending')                │
       │                                                       │                │
       │                              ┌── video_finalize worker ─────────────►  │
       │                              │ measure dir = _dir_size(segments/{O})
       │                              │ delta = actual - reserved - old
       │                              │   delta>0: atomic reserve(+delta)
       │                              │     ok → status='done', transcoded_bytes=actual
       │                              │     no → reap segments,
       │                              │           refund FULL reserved,
       │                              │           status='failed_quota' (Flow 4b)
       │                              │   delta≤0: refund (-delta),
       │                              │             status='done'
       │                              └───────────────────────────────────────►
```

### Flow 4a — pre-flight skip (no GPU time wasted)

```
estimate = 1.2 GB, available_quota = 200 MB
→ ATOMIC QuotaReserve rejects
→ UPDATE objects SET transcode_status='skipped_quota',
                     transcoded = '{"quota":{"estimatedBytes":..,
                                            "deficitBytes":..}}'::jsonb
→ no transcode_jobs rows ever created
```

### Flow 4b — publish-point fail (GPU time spent, output discarded)

```
estimate = 600 MB, actual segments = 1.1 GB, only 200 MB headroom left
→ delta = 1.1GB - 600MB - 0 = 500 MB
→ ATOMIC reserve(+500 MB) rejects
→ shutil.rmtree(/storage/segments/{O}/)
→ refund full 600 MB reservation
→ UPDATE objects SET transcode_status='failed_quota',
                     transcoded_bytes = 0,
                     transcode_reserved_bytes = 0
```

### Cancel-on-delete

The API publishes to Valkey channel `transcode:cancel` whenever an
object is deleted (purged, soft-deleted to trash, or "Cancel & restart").
The worker's subscriber thread receives `{"objectId":"..."}`, looks up
running FFmpeg by object_id, and SIGTERMs it. The worker thread then
cleans up via the `fail_job` path which refunds the reservation.

The browser polls `/api/{bucket}/{key}?info` (1s while pending) to know
when transcoding finishes (`transcodeStatus: "done"`). At that point the
video player appears with HLS adaptive bitrate switching.

---

## Flow 5: Watch the transcoded video (HLS playback)

```
Browser (video.js)         nginx                  /storage/segments/{id}/
       │                     │                              │
       │ GET /stream/{id}/master.m3u8                       │
       ├────────────────────►│ alias /storage/segments/    │
       │                     │ readfile master.m3u8        │
       │                     │◄─────────────────────────────│
       │ Content-Type: application/vnd.apple.mpegurl       │
       │ Cache-Control: max-age=60                         │
       │ Access-Control-Allow-Origin: *                    │
       │◄────────────────────┤                              │
       │                                                    │
       │ video.js picks initial quality based on bandwidth │
       │ (typically starts at 720p)                        │
       │                                                    │
       │ GET /stream/{id}/720p/playlist.m3u8                │
       ├────────────────────►│ readfile 720p/playlist.m3u8 │
       │                     │◄─────────────────────────────│
       │                                                    │
       │ Parse playlist → list of segment_000.ts, 001.ts ...│
       │                                                    │
       │ GET /stream/{id}/720p/segment_000.ts               │
       ├────────────────────►│ sendfile() → kernel socket  │
       │  (672KB of MPEG-TS) │◄─────────────────────────────│ (zero-copy)
       │                     │ Cache-Control:              │
       │                     │   public, max-age=31536000, │
       │                     │   immutable                 │
       │◄────────────────────┤                              │
       │                                                    │
       │ playback begins, video.js prefetches next segments │
       │                                                    │
       │ if bandwidth degrades → switch to 480p mid-stream │
       │ if widens → switch up to 1080p                    │
       │                                                    │
       │ With Cloudflare in front, segments after the      │
       │ first request are served from the nearest edge    │
       │ POP, not your laptop.                             │
```

---

## Flow 6: AWS CLI uses your storage (SigV4)

```
aws-cli (--endpoint-url=https://s3.yourdomain.com)        Cloudflare           nginx              api
    │                                                         │                  │                 │
    │ aws s3 cp file.txt s3://photos/file.txt                                    │                 │
    │                                                         │                  │                 │
    │ Compute SigV4:                                                             │                 │
    │   canonical request = method+path+query+headers+payloadhash                │                 │
    │   stringToSign = "AWS4-HMAC-SHA256\n" + date + scope + sha256(canonical) │                 │
    │   signingKey = HMAC chain from AWS4+secret                                 │                 │
    │   signature = HMAC(signingKey, stringToSign)                              │                 │
    │                                                                            │                 │
    │ PUT /photos/file.txt                                                       │                 │
    │ Authorization: AWS4-HMAC-SHA256 Credential=AKID/.../s3/aws4_request,      │                 │
    │   SignedHeaders=host;x-amz-content-sha256;x-amz-date,                     │                 │
    │   Signature=<hex>                                                          │                 │
    │ X-Amz-Date: 20260529T120000Z                                              │                 │
    │ X-Amz-Content-Sha256: <hex of body>                                       │                 │
    ├────────────────────────────────────────────────────────►│                  │                 │
    │                                                         │ tunnel forward   │                 │
    │                                                         ├─────────────────►│                 │
    │                                                                            │ proxy_pass +    │
    │                                                                            │ X-Original-URI: │
    │                                                                            │ /photos/file.txt│
    │                                                                            ├────────────────►│
    │                                                                            │                 │ Authenticator detects
    │                                                                            │                 │   "AWS4-HMAC-SHA256" prefix
    │                                                                            │                 │ ParseAuthHeader → AKID, scope, sig
    │                                                                            │                 │ SELECT secret FROM s3_credentials
    │                                                                            │                 │   WHERE access_key_id = ?
    │                                                                            │                 │ VerifySigV4:
    │                                                                            │                 │   recompute signature
    │                                                                            │                 │   compare in constant time
    │                                                                            │                 │ load *User, attach to ctx
    │                                                                            │                 │
    │                                                                            │                 │ (proceed exactly like Flow 2:
    │                                                                            │                 │  CheckDiskHealthy, QuotaReserve,
    │                                                                            │                 │  WriteObject, INSERT objects,
    │                                                                            │                 │  enqueueTranscode)
    │                                                                            │                 │
    │                                                                            │ 200 ETag: ".."  │
    │                                                                            │◄────────────────┤
    │                                                         │◄─────────────────┤                 │
    │ 200                                                     │                  │                 │
    │◄────────────────────────────────────────────────────────┤                  │                 │
```

---

## See also

- [low-level.md](./low-level.md) for each component's internals
- [database-schema.md](./database-schema.md) for the tables involved
