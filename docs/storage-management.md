# Storage Management

Where files live, how the system tracks capacity, and how to scale.

## What's on disk

```
$STORAGE_ROOT/                              ← bind-mounted in compose
├── buckets/
│   └── {bucket-uuid}/
│       ├── objects/
│       │   └── {sha256-of-key}/            ← deterministic path
│       │       ├── data                    ← the actual file bytes
│       │       ├── data.tmp (transient)    ← mid-write; cleaner gates by mtime
│       │       └── versions/{vid}          ← versioned bucket snapshots
│       └── tmp/
│           └── {upload-id}/                ← in-progress multipart
│               ├── part-00001
│               ├── part-00002
│               └── ...
└── segments/
    └── {object-uuid}/                      ← transcoder output
        ├── master.m3u8
        ├── 2160p/playlist.m3u8 + segment_*.ts   (dynamic ladder; 4K source
        ├── 1440p/playlist.m3u8 + segment_*.ts    gets 5+ rungs, 720p source
        ├── 1080p/playlist.m3u8 + segment_*.ts    only gets 720p/480p/360p)
        ├── 720p/playlist.m3u8 + segment_*.ts
        ├── 480p/playlist.m3u8 + segment_*.ts
        ├── 360p/playlist.m3u8 + segment_*.ts
        └── thumb_0.jpg, thumb_1.jpg, ...
```

Anything outside this layout — a stray file dropped into `buckets/{B}/`,
a hash dir whose object doesn't exist, a segments dir for a deleted object —
is treated as an orphan and reaped by the cleaner. See
[`come-up-designs/cleaner.md`](./come-up-designs/cleaner.md) for the V4
adaptive Merkle-trie design.

**Key naming:** Object keys can contain slashes (`videos/2024/holiday.mp4`)
but they aren't real directories — the whole string is the key. We hash
the key with SHA-256 to get a flat per-bucket layout that doesn't suffer
from huge directory listings.

---

## Per-user quota model

`users.used_bytes` is an atomically-maintained running total. It equals:

```
used_bytes = SUM(objects.size_bytes
              + objects.transcoded_bytes
              + objects.transcode_reserved_bytes)
           + SUM(object_versions.size_bytes)
                                          ← all scoped to this user's buckets
```

Three categories per object:

| Column | What | Charged when | Refunded when |
|---|---|---|---|
| `size_bytes` | Original upload | `PUT` complete / multipart `Complete` | object purged from trash |
| `transcoded_bytes` | HLS segments on disk | worker finalize publishes | transcode deleted, object purged |
| `transcode_reserved_bytes` | Pre-flight estimate | API enqueues transcode pipeline | worker settles delta at publish, or refunds on cancel/fail/skip |

### Transcode quota lifecycle

```
Upload media → object row exists (size_bytes charged) → transcode_status='none'
       │
       │  Bucket auto_transcode_mode != off
       ▼
API EnqueueTranscode:
  1. probe duration via ffprobe
  2. estimate = Σ(rung.VBR + rung.ABR) × duration × 1.4   ← safety factor
  3. ATOMIC QuotaReserve(+estimate)      ← closes the race against
                                            concurrent uploads
       │
       ├── insufficient → status='skipped_quota',
       │                  no jobs enqueued,
       │                  diagnostic in objects.transcoded.quota
       │
       └── success → transcode_reserved_bytes = estimate,
                     status='pending', N+2 jobs queued (rungs+thumbs+finalize)

Worker picks up:
  status → 'processing'
  ffmpeg encodes; writes /storage/segments/{id}/...

Worker video_finalize publishes:
  actual = _dir_size(segments_dir)
  delta  = actual - reserved - old_transcoded_bytes
    delta > 0 → ATOMIC reserve(+delta)
                  success → transcoded_bytes=actual, reserved=0, status='done'
                  failure → reap segments dir,
                            refund FULL reservation,
                            status='failed_quota',
                            diagnostic in objects.transcoded.quota
    delta ≤ 0 → refund (-delta), transcoded_bytes=actual, reserved=0, status='done'

Worker fail_job (attempts ≥ max_attempts):
  refund reserved, reap segments, status='failed'

API DELETE /:b/:k?transcodes (Cancel & restart):
  refund (transcoded_bytes + reserved), reap segments, status='none'
```

### Why two layers (pre-flight + publish-point enforcement)

- **Pre-flight saves CPU.** A 30s 4K clip going through 5 rungs is ~5 minutes
  of GPU work. Catching "won't fit" upfront skips it entirely.
- **Publish-point is the hard guarantee.** The estimate has a 1.4 safety
  factor but real output varies with codec efficiency. If actual ≠ estimate,
  publish-point enforces the cap regardless.
- **Together they close the TOCTOU.** Pre-flight *reserves*; concurrent
  uploads see each other's reservations debited from `used_bytes`.

### Surfacing it to clients

`GET /auth/storage` returns the per-bucket + trash + versions + reserved
breakdown the dashboard uses for its stacked chart. Quota-rejection
errors (`507 QUOTA_EXCEEDED`) include a `details` block:

```json
{
  "code": "QUOTA_EXCEEDED",
  "message": "part would exceed your storage quota",
  "details": {
    "requestedBytes": 5242880,
    "usedBytes":      2014871552,
    "quotaBytes":     2147483648,
    "availableBytes": 132612096,
    "deficitBytes":   0
  }
}
```

For `skipped_quota` / `failed_quota` the same shape lives in
`objects.transcoded.quota` (no separate HTTP error — it's an async outcome).

### Drift safety net

`scripts/quota-reconcile.sql` recomputes `users.used_bytes` from the
authoritative sum above. **Idempotent — run weekly via cron**
(snippet in [production-tuning.md](./production-tuning.md)).

```bash
docker compose exec -T postgres psql -U s3admin -d personals3 \
  -f - < scripts/quota-reconcile.sql
```

It prints the drift before correcting, then UPDATEs. Healthy systems
should show `drift = 0 bytes` for every user.

---

## Three different "totals" to understand

```
┌─────────────────────────────────────────────────────┐
│  PHYSICAL DISK                                      │
│  what `df -h /storage` shows                        │
│                                                     │
│  ┌──── OS / DB / logs / system  ────┐               │
│  │ reserved_bytes (configurable)    │               │
│  └──────────────────────────────────┘               │
│                                                     │
│  ┌──── effective total ─────────────────────┐       │
│  │ what the system will allocate to users   │       │
│  │ = total_quota_bytes (if set)             │       │
│  │   OR physical total - reserved_bytes     │       │
│  │                                          │       │
│  │  ┌── allocated to users ──┐              │       │
│  │  │ sum of users.quota_bytes              │       │
│  │  │                                       │       │
│  │  │  ┌── actually used ──┐                │       │
│  │  │  │ sum of users.used_bytes            │       │
│  │  │  └────────────────────┘               │       │
│  │  └────────────────────────┘              │       │
│  └─────────────────────────────────────────┘       │
└─────────────────────────────────────────────────────┘
```

You can see all four in real time at **Admin → System → Storage**.

---

## Knobs

All in **Admin → System → Storage Configuration**:

| Setting | Default | Effect |
|---|---|---|
| `total_quota_bytes` | `0` | System cap. `0` = auto-detect from physical disk minus reserved |
| `reserved_bytes` | `5 GB` | Headroom for OS / DB / logs / future segments |
| `disk_full_threshold_pct` | `95` | Reject uploads when disk past this % (regardless of per-user quota) |
| `overcommit_allowed` | `false` | If true, admin can grant more quota than the disk has |

---

## Common operations

### See what the disk looks like right now

**Via dashboard:** Admin → System → Storage panel.

**Via shell:**
```bash
df -h ~/Projects/S3orSimilar/storage
docker compose exec api wget -q -O- http://localhost:3000/healthz
```

**Via SQL (sum across all users):**
```bash
docker compose exec postgres psql -U s3admin -d personals3 -c "
  SELECT COUNT(*) AS users,
         pg_size_pretty(SUM(quota_bytes)) AS total_allocated,
         pg_size_pretty(SUM(used_bytes))  AS actual_used
    FROM users;"
```

---

### Grow capacity — option A: bigger physical disk

The system auto-detects via `statfs(2)`. Expand the underlying filesystem
and the new capacity appears.

```bash
# Example: extending an LVM volume
sudo lvextend -L +500G /dev/vg0/lvstorage
sudo resize2fs /dev/vg0/lvstorage      # for ext4
# or
sudo xfs_growfs /storage               # for xfs

# Restart the API so it re-reads disk stats on next request
docker compose restart api
# (or just wait — the admin page polls every 5s and recalculates)
```

---

### Grow capacity — option B: move to a different disk

```bash
# 1. Stop everything
docker compose down

# 2. Copy data to the new disk
sudo cp -a ~/Projects/S3orSimilar/storage /mnt/bigdisk/personals3-data
sudo chown -R $USER:$USER /mnt/bigdisk/personals3-data

# 3. Update .env
# STORAGE_ROOT=/mnt/bigdisk/personals3-data

# 4. Restart
docker compose up -d
```

The PostgreSQL `storage_path` column for each object stays valid because it's
referenced through the mount, not the underlying disk.

---

### Cap below physical (reserve some for other apps)

Admin → System → Storage Configuration → set `total_quota_bytes` to e.g.
`500` (GB). The system will refuse to grant more than 500 GB total user
quota, even if the disk is bigger.

This is useful if you share the disk with other services and want a hard
ceiling on what PersonalS3 can claim.

---

### Allow overcommit (sum of quotas > disk)

When most users underutilize, you can promise more quota than you have:

Admin → System → Storage Configuration → toggle `overcommit_allowed` to `true`.

The system will then allow `sum(users.quota_bytes) > effective_total`. Each
user's hard ceiling is still their own `quota_bytes` — overcommit just lets
the *sum* go higher.

Disk-full protection still kicks in (`disk_full_threshold_pct`), so you
won't ever fill the disk past 95% even with overcommit.

---

### Reclaim space

Storage is freed automatically when:
- An object is purged (`DELETE /:bucket/:key?purge` or empty-from-trash)
  — refunds `size_bytes + transcoded_bytes + transcode_reserved_bytes + Σ versions`
- A bucket is deleted (cascades to all objects; quota refunded per row)
- The cleaner reaps orphans (see below)

The **cleaner (V4)** runs every 30 s and reaps automatically:

| What | Detected by | When |
|---|---|---|
| `buckets/{B}/objects/{H}/data` whose hash isn't in any object's `sha256(key)` | bloom membership check in `walkShard` | second tick after sighting (two-strike) |
| `buckets/{B}/objects/{H}/data.tmp` older than `OrphanMinAge` | mtime gate in `walkShard` | next tick after gate passes |
| `segments/{O}/*` whose object is not in `('done','pending','processing')` and `transcoded_bytes=0` | filtered bloom in `buildBloomFilters` | next tick |
| `buckets/{B}/tmp/{U}/*` whose multipart upload row is gone or expired (7d) | bloom membership + `multipart_sweep` | next tick |
| Stray non-hash entries in `buckets/{B}/objects/` and `buckets/{B}/` root | name-shape check in `walkShard` + `sweepBucketRoots` | second tick (two-strike) |

Tuning knobs in `.env`:

```
ORPHAN_MIN_AGE_MINUTES=30    # files must be older than this (race protection)
ORPHAN_TWO_STRIKE=1          # require two sightings before reaping (prod default)
CLEANUP_DRY_RUN=0            # 1 = log candidates but don't delete
```

Audit each tick with:

```bash
docker compose exec -T postgres psql -U s3admin -d personals3 -c "
  SELECT id, started_at, reaped_counts, bytes_freed, errors
    FROM cleanup_runs ORDER BY started_at DESC LIMIT 5;"
```

For a one-shot inventory of segments-vs-objects mismatch:

```bash
# Run inside the api container so paths line up
docker compose exec api sh -c '
  for d in /storage/segments/*/; do
    oid=$(basename "$d")
    psql "$DATABASE_URL" -tA -c "SELECT 1 FROM objects WHERE id='\''$oid'\''
      AND transcode_status IN ('\''done'\'','\''pending'\'','\''processing'\'')" | grep -q 1 \
      || echo "ORPHAN: $d"
  done'
```

The cleaner will reap any `ORPHAN:` line within ~60 s.

---

## Monitoring (alerts)

The dashboard's overview page polls `/auth/storage` every 8 s and renders
a stacked bar grouped by bucket, with separate slices for trash,
transcode reservations, and free space. For external monitoring:

```bash
# Per-user quota state
docker compose exec -T postgres psql -U s3admin -d personals3 -tA -c "
  SELECT email,
         used_bytes::float / NULLIF(quota_bytes,0) * 100 AS used_pct
    FROM users ORDER BY used_pct DESC NULLS LAST;"

# System-wide disk + allocation (admin)
RESPONSE=$(curl -s -H "Authorization: Bearer $ADMIN_KEY" \
  http://localhost:8080/api/admin/stats)

PHYS_PCT=$(echo "$RESPONSE" | jq '.disk.physicalUsed * 100 / .disk.physicalTotal')
ALLOC_PCT=$(echo "$RESPONSE" | jq '.disk.allocatedToUsers * 100 / .disk.effectiveTotal')
USED_PCT=$(echo "$RESPONSE" | jq '.disk.actuallyUsed * 100 / .disk.allocatedToUsers')

echo "Physical disk: ${PHYS_PCT}%"        # alert > 80
echo "Allocated:     ${ALLOC_PCT}%"       # alert > 90
echo "Used:          ${USED_PCT}%"        # informational

# Quota drift across all users — should be 0 bytes after weekly reconcile
docker compose exec -T postgres psql -U s3admin -d personals3 -c "
  SELECT email,
         pg_size_pretty(used_bytes
           - ((SELECT COALESCE(SUM(size_bytes + COALESCE(transcoded_bytes,0)
                                   + COALESCE(transcode_reserved_bytes,0)),0)
                 FROM objects
                WHERE bucket_id IN (SELECT id FROM buckets WHERE owner_id=u.id))
            + (SELECT COALESCE(SUM(ov.size_bytes),0)
                 FROM object_versions ov JOIN objects o ON o.id=ov.object_id
                WHERE o.bucket_id IN (SELECT id FROM buckets WHERE owner_id=u.id)))
         ) AS drift
    FROM users u
   ORDER BY u.email;"
```

If `drift` is non-zero anywhere, run `scripts/quota-reconcile.sql`.
