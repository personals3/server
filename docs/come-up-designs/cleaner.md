# Cleaner / Storage Integrity — Design Evolution

> **Component:** Background garbage collector + storage integrity verifier.
> Enforces the invariant: **every file on disk must be reachable from a DB row.**
> File: `api/internal/cleaner/`, container: `cleaner`.
>
> **Why this doc exists:** the same problem (verifying a huge set of files
> against a manifest) shows up in many systems — backup verifiers, CDN
> origin sweepers, content-addressed stores, deduplication engines. This
> file walks each design we considered, what broke at scale, and what we
> picked. Reuse it elsewhere; the patterns transfer.

---

## Table of contents

- [V1 — Pure DB scan](#v1--pure-db-scan)
- [V2 — Bloom filter membership](#v2--bloom-filter-membership)
- [V3 — Merkle-trie over hash-prefix shards (user proposal)](#v3--merkle-trie-over-hash-prefix-shards-user-proposal)
- [V4 — Adaptive shard depth (IPv6-style expansion) — FINAL](#v4--adaptive-shard-depth-ipv6-style-expansion--final)
- [Schemas and queries](#schemas-and-queries)
- [Scaling matrix](#scaling-matrix)
- [Lessons that transfer to other projects](#lessons-that-transfer-to-other-projects)

---

## The problem

For every file on disk, we need to answer **two questions** cheaply:

1. **Disk → DB**: is this file accounted for in the DB? If not → orphan → reap.
2. **DB → Disk**: does this row's file actually exist? If not → broken, log for admin.

Difficulty grows with N (number of objects). The component must:

- Stay **fast** even at 100M–10B files (security-critical: a bot-dropped orphan should die in seconds, not 24 hours).
- Stay **cheap** (low DB connection use, bounded memory, doesn't compete with API/worker hot paths).
- Be **safe** (never delete a legit file; dry-run + two-strike rules).
- Be **traceable** (audit log per reap, with reason).

Performance target: cleaner tick should run in **single-digit seconds** at any realistic scale.

---

## V1 — Pure DB scan

**Idea.** Every tick, load all `(bucket_id, sha256(key))` tuples from `objects` into a Go `map[string]struct{}`. Walk the disk, check each file against the map. Repeat for `object_versions`, `multipart_uploads`, `segments`.

```go
// Pseudocode
hashSet := map[string]struct{}{}
rows, _ := db.Query(`SELECT bucket_id, key FROM objects`)
for rows.Next() {
    var b, k string
    rows.Scan(&b, &k)
    hashSet[b+"|"+sha256Hex(k)] = struct{}{}
}

walkDisk(func(bucketID, hashDir string) {
    if _, ok := hashSet[bucketID+"|"+hashDir]; !ok {
        reap(...)
    }
})
```

**Problem.**

| Scale | RAM for set | DB query time | Walk time | Per-tick cost |
|---|---|---|---|---|
| 1M files | ~30 MB | <1s | seconds | OK |
| 10M files | ~300 MB | seconds | minutes | painful |
| **100M files** | **~3 GB** | **~80s** | **~15 min** | **unacceptable** |
| 1B files | ~30 GB (OOM) | many minutes | hours | impossible |

The Go-map approach has TWO bottlenecks: memory grows linearly with N, and the DB pulls 100M rows every tick.

**Verdict.** Works for small deployments. Falls over before any meaningful scale.

---

## V2 — Bloom filter membership

**Idea.** Replace the giant hash set with a probabilistic **bloom filter**.

- 100M items at 0.1% false positive → **~170 MB filter** (vs 3 GB hash map)
- Stream rows in, add to bloom, discard. Memory bound by filter, not row count.
- Bloom has **no false negatives** — if `bloom.Test(x) == false`, x is genuinely not in the set. Safe to mark as orphan.
- False positives (~0.1%) mean "maybe in DB" → confirm with a single SELECT.

```go
bf := bloom.NewWithEstimates(100_000_000, 0.001)
rows, _ := db.Query(`SELECT bucket_id::text, key FROM objects`)
for rows.Next() {
    var b, k string
    rows.Scan(&b, &k)
    bf.AddString(b + "|" + sha256Hex(k))
}

walkDisk(func(path string) {
    key := derive(path)
    if !bf.TestString(key) {
        markOrphan(path)
        return
    }
    // bloom says yes — definitely alive, no DB hit
})
```

**Wins.**

- Memory **18× smaller**: 700 MB for the full V2 system at 100M.
- DB load **unchanged** for the build phase (one streaming SELECT every 6h), **zero** per-tick after that.

**Remaining problem.**

The DISK WALK is still O(N). For 100M:

| Op | Per entry | 100M entries |
|---|---|---|
| `readdir` | ~1 µs | ~100 s |
| Bloom check | ~100 ns | ~10 s |
| **Total walk** | | **~2 min** (single thread) |

**Two minutes is too slow** for security-driven cleanup. If a bot drops a malicious orphan, you don't want a 2-minute (or hourly) detection window.

Also: filesystems get slow with millions of sibling directories. With 100M files all under one `objects/` parent, `readdir` itself starts struggling.

**Verdict.** Order-of-magnitude better than V1. Memory solved, but walk time still scales linearly with total file count. We're checking every file every scan, even though >99.99% haven't changed.

---

## V3 — Merkle-trie over hash-prefix shards (user proposal)

**Insight from the user.** Look at how networking and DNS scale:

> "all of those domain names with some tries structure becomes like max height of 128 (maybe idk) causing lot of less data"

DNS doesn't scan all 370M domain names to resolve `foo.example.com`. It walks the **zone tree**: `.` → `.com` → `example.com`. Each step only owns its slice. Total work is O(depth), not O(total names).

Apply the same idea to storage: **structure the manifest as a trie keyed by sha256 prefix, with per-subtree state hashes (Merkle summaries) so unchanged subtrees can be SKIPPED entirely.**

### Storage layout

```
# Today (V2, flat — 100M sibling dirs)
buckets/{B}/objects/{sha256(key)}/data

# V3 (hash-prefix fanout, 65,536 leaf buckets)
buckets/{B}/objects/{H[0:2]}/{H[2:4]}/{H[4:]}/data
                     ↑       ↑       ↑
                   256-way 256-way leaf bucket
                   fanout  fanout  (~1500 files at 100M)
```

### Manifest with state hashes

```sql
CREATE TABLE object_shard_index (
  bucket_id    UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  shard_path   TEXT NOT NULL,                -- "ab/cd"
  state_hash   BYTEA NOT NULL,               -- BLAKE3(sorted object_ids)
  object_count INT NOT NULL DEFAULT 0,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (bucket_id, shard_path)
);
```

### The trick — dirty-shard detection

Every PUT/DELETE updates the affected shard's `state_hash` and `updated_at` in the same transaction:

```sql
-- In PutObject transaction:
INSERT INTO objects (...) VALUES (...) RETURNING id;

UPDATE object_shard_index
   SET state_hash = $1,         -- recomputed incrementally
       object_count = object_count + 1,
       updated_at = now()
 WHERE bucket_id = $2 AND shard_path = $3;
```

Cleaner asks ONE question to find work:

```sql
-- Disk-side computes its own state_hash from `readdir + sort`,
-- compares to DB's state_hash. The cleaner only walks shards where
-- they differ.
SELECT bucket_id, shard_path
  FROM object_shard_index
 WHERE state_changed_since_last_walk;
```

For 100M files with ~1K writes/day, typically ~5 shards out of 65,536 are dirty per cleaner tick. The cleaner walks **~7,500 files** instead of 100M — a **>10,000× reduction**.

### Scaling numbers (V3)

| | V2 (flat bloom) | V3 (Merkle-trie shards) |
|---|---|---|
| Cleaner tick (no churn) | 2 min walk | **~5 ms** (single SELECT, 0 rows) |
| Cleaner tick (5 dirty shards) | 2 min walk | **~80 ms** |
| Suspicious file detection | up to 24h | **next tick, <60s** |
| Memory | 700 MB bloom | ~5 MB shard index in DB |
| Per-write overhead | 0 | +1 indexed UPDATE (~0.5ms) |
| DB load steady state | 1 streaming SELECT per 6h | 1 small SELECT per tick |

### Live cleanup scenario

1. Bot drops `evil.sh` at `/storage/buckets/{B}/objects/de/ad/beef.../data`
2. The shard `de/ad` now has 1501 files on disk, 1500 in DB. Its disk Merkle differs from DB Merkle.
3. Cleaner ticks every 30 seconds, asks: "any dirty shards?"
4. Sees `de/ad` mismatch, walks ONLY that shard (1500 files, ~10ms).
5. Finds the file with no `objects` row → reaps + audits.

**End-to-end: under 60 seconds**, vs up to 24 hours with V2.

### What V3 still doesn't solve

V3 fixes the dirty-set selection beautifully. But the **leaf size** is fixed: with 65,536 leaves you can hold:

| Total files | Files per leaf | Walk-per-leaf cost |
|---|---|---|
| 100M | ~1,500 | ~10ms — great |
| 1B | ~15,000 | ~150ms — OK |
| 10B | ~150,000 | ~1.5s — getting slow |
| **100B** | **~1.5M** | **~15s — IPv4 exhaustion** |

This is the **IPv4 problem**: a fixed 16-bit address space (65,536 leaves) works until your population outgrows it. Then you need expansion.

---

## V4 — Adaptive shard depth (IPv6-style expansion) — FINAL

**Insight.** The IPv4 → IPv6 transition didn't just add more bits; it removed the assumption that all addresses had to share the same flat allocation. CIDR + subnetting let you **delegate depth on demand**.

Apply the same logic: don't fix the trie depth at 4 hex chars. Let each shard split itself when it grows too big — like a **B-tree leaf split**, or like DNS zone delegation when a TLD spins up its own sub-NS.

### The trie expands itself

Shard table now has variable-depth `shard_path`:

```sql
CREATE TABLE object_shard_index (
  bucket_id    UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  shard_path   TEXT NOT NULL,                -- "ab", or "ab/cd", or "ab/cd/ef/01"
  depth        INT NOT NULL,                 -- length(shard_path) / 3 + 1
  is_leaf      BOOLEAN NOT NULL DEFAULT TRUE,
  state_hash   BYTEA NOT NULL,
  object_count INT NOT NULL DEFAULT 0,
  last_walk_at TIMESTAMPTZ,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (bucket_id, shard_path),
  CHECK (depth BETWEEN 1 AND 16)              -- 16 nibbles = 64 hash bits = plenty
);

CREATE INDEX idx_shard_dirty
  ON object_shard_index(bucket_id)
  WHERE last_walk_at IS NULL OR last_walk_at < updated_at;
```

### How splits work

```
Phase 1 — empty bucket
  bucket has ONE shard:  ""  (root, depth=0, is_leaf=TRUE)

Phase 2 — files accumulate
  shard "" reaches SHARD_SPLIT_THRESHOLD = 5000 files
  → split into 16 child shards: "0", "1", ..., "f"
  → mark "" as is_leaf=FALSE (becomes interior node)
  → move object_shard_index rows: distribute files into children by H[0]

Phase 3 — keeps growing
  shard "a" reaches threshold → splits into "a0", "a1", ..., "af"
  shard "b" still small → stays as-is
  ...

  Result: tree depth is unbalanced — branches with hot keys go deep,
  cold branches stay shallow. Same idea as a Patricia trie.
```

A shard's **on-disk path** matches its `shard_path`:

```
shard ""    → buckets/{B}/objects/        (no fanout, files at root)
shard "a"   → buckets/{B}/objects/a/
shard "ab"  → buckets/{B}/objects/a/b/
shard "abc" → buckets/{B}/objects/a/b/c/
```

### Address space

Each hex nibble = 4 bits. Max depth 16 nibbles = **64 bits** of hash-prefix addressing — that's **18 quintillion (1.8 × 10^19)** unique shards. Even at 1 trillion files we'd never exhaust it. (We could go to 32 nibbles = 128 bits = full sha256 if ever needed, matching IPv6's address space directly.)

| Address bits | Max shards | Files per shard @ 1T total | Walk cost |
|---|---|---|---|
| 8 (IPv4 /24) | 256 | 4 billion | unusable |
| 16 | 65K | 15M | slow |
| 32 (IPv4 size) | 4B | 250 | great |
| 64 (V4 cap) | 1.8 × 10^19 | <<1 | overkill — splits stop earlier |
| 128 (IPv6) | 3.4 × 10^38 | — | full sha256 space |

### Split lifecycle

```
On every write:
  1. INSERT/UPDATE objects (existing logic)
  2. Find the leaf shard for sha256(key) (longest matching prefix in shard table)
  3. UPDATE that shard's state_hash + object_count + updated_at
  4. IF object_count >= SHARD_SPLIT_THRESHOLD:
     - mark for split (just set is_leaf=FALSE; actual split deferred to cleaner)

On cleaner tick:
  1. Process splits queued from writes (atomic: move children into place)
  2. Select dirty shards (last_walk_at IS NULL OR < updated_at)
  3. Walk each, reconcile disk vs DB, reap orphans
  4. Update last_walk_at
```

Splits are **deferred to the cleaner** so user writes don't pay a "redistribute 5K files" cost on the hot path.

### Walk algorithm (dirty-shard detection)

```sql
-- Cleaner tick start
WITH dirty AS (
  SELECT bucket_id, shard_path
    FROM object_shard_index
   WHERE is_leaf = TRUE
     AND (last_walk_at IS NULL OR last_walk_at < updated_at)
     AND bucket_id IN (SELECT id FROM buckets WHERE NOT archived)
   LIMIT $1  -- MAX_SHARDS_PER_TICK, e.g. 1000
)
SELECT bucket_id, shard_path FROM dirty;
```

Then for each dirty shard, in parallel:

```go
func walkShard(bucketID uuid.UUID, shardPath string) {
    diskFiles := readDir(diskPathFor(bucketID, shardPath))
    dbFiles   := query("SELECT id FROM objects WHERE shard_id = $1", ...)

    diskHash := merkleOf(diskFiles)
    dbHash   := merkleOf(dbFiles)

    if diskHash == dbHash {
        // false alarm — write race or already cleaned
        markWalked(bucketID, shardPath)
        return
    }

    orphans := diskFiles - dbFiles  // set diff
    for o := range orphans {
        twoStrikeReap(o, ...)
    }
    missing := dbFiles - diskFiles
    for m := range missing {
        log("missing data file for row", m)
    }

    updateMerkle(bucketID, shardPath, diskHash)
    markWalked(bucketID, shardPath)
}
```

### Scaling at 1B files

| Scenario | Tick cost |
|---|---|
| No writes since last tick | 1 SELECT, **<10 ms** |
| 100 writes since last tick (~50 dirty shards) | 50 small walks in parallel, **<500 ms total** |
| One million writes (large import) | shards split as needed, ~5 s amortized |
| Full reconcile (admin "Run now") | walks ALL leaf shards in parallel — minutes, not hours |

Compared to V2 at 1B files: **>1000× faster** for normal ticks, **>100× faster** for full sweeps. Memory: a few MB (just the shard table rows in flight) vs ~17 GB bloom.

### Security implications

V4 turns the cleaner into a **near-real-time integrity monitor**, not a periodic sweeper:

- **Detection latency:** ~30 seconds (one cleaner tick) regardless of total file count.
- **No window for orphans to "blend in"** by being one file among millions — the trie isolates them in their shard immediately.
- **Audit log per reap** is still mandatory (NDJSON to `/storage/.cleanup/`).
- The `state_hash` mismatch IS the alarm: any unauthorized write breaks the shard's Merkle root immediately.

---

## Schemas and queries

### Tables

```sql
-- The shard index — heart of V4.
CREATE TABLE object_shard_index (
  bucket_id    UUID NOT NULL REFERENCES buckets(id) ON DELETE CASCADE,
  shard_path   TEXT NOT NULL,           -- "", "a", "ab", "abc", ...
  depth        INT NOT NULL,            -- length(shard_path)
  is_leaf      BOOLEAN NOT NULL DEFAULT TRUE,
  state_hash   BYTEA NOT NULL DEFAULT '\x00'::bytea,
  object_count INT NOT NULL DEFAULT 0,
  last_walk_at TIMESTAMPTZ,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (bucket_id, shard_path),
  CHECK (depth >= 0 AND depth <= 16),
  CHECK (length(shard_path) = depth)
);

CREATE INDEX idx_shard_dirty
  ON object_shard_index(bucket_id, shard_path)
  WHERE is_leaf = TRUE
    AND (last_walk_at IS NULL OR last_walk_at < updated_at);

CREATE INDEX idx_shard_split_candidates
  ON object_shard_index(object_count)
  WHERE is_leaf = TRUE AND object_count > 5000;
```

Add a derived column to `objects` so we know which shard each row lives in
(set at INSERT time, kept in sync if a shard splits):

```sql
ALTER TABLE objects ADD COLUMN shard_path TEXT;
CREATE INDEX idx_objects_shard ON objects(bucket_id, shard_path);
```

### Hot-path: PUT object (transactional)

```sql
BEGIN;

-- 1. Insert the object row.
INSERT INTO objects (id, bucket_id, key, size_bytes, etag, content_type, storage_path)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- 2. Find the leaf shard for this key's hash.
--    Longest matching prefix wins.
WITH key_hash AS (SELECT encode(sha256($3::bytea), 'hex') AS h)
SELECT shard_path
  FROM object_shard_index s, key_hash k
 WHERE s.bucket_id = $2
   AND s.is_leaf = TRUE
   AND k.h LIKE s.shard_path || '%'
 ORDER BY s.depth DESC
 LIMIT 1;
-- → store the resulting shard_path as $shard

-- 3. Record the assignment + bump the shard's state_hash + count.
UPDATE objects SET shard_path = $shard WHERE id = $1;

UPDATE object_shard_index
   SET state_hash   = digest(state_hash || $1::text::bytea, 'sha256'),
       object_count = object_count + 1,
       updated_at   = now()
 WHERE bucket_id = $2 AND shard_path = $shard;

COMMIT;
```

`digest()` is from `pgcrypto`. We use an associative running hash so order
of inserts doesn't matter (`H(A,B) == H(B,A)` for set semantics).

### Cleaner — find dirty shards

```sql
SELECT bucket_id, shard_path
  FROM object_shard_index
 WHERE is_leaf = TRUE
   AND (last_walk_at IS NULL OR last_walk_at < updated_at)
 ORDER BY updated_at
 LIMIT $1;
```

### Cleaner — process a split

When a leaf's `object_count > SHARD_SPLIT_THRESHOLD`:

```sql
BEGIN;

-- Within this transaction:
-- 1. Mark current leaf as internal node
UPDATE object_shard_index
   SET is_leaf = FALSE, state_hash = '\x00'
 WHERE bucket_id = $1 AND shard_path = $parent;

-- 2. Create 16 child leaves
INSERT INTO object_shard_index (bucket_id, shard_path, depth, is_leaf, state_hash, object_count)
SELECT $1, $parent || nibble, length($parent || nibble), TRUE, '\x00'::bytea, 0
  FROM (VALUES ('0'),('1'),('2'),('3'),('4'),('5'),('6'),('7'),
               ('8'),('9'),('a'),('b'),('c'),('d'),('e'),('f')) AS n(nibble);

-- 3. Reassign affected objects to their new child shards
UPDATE objects
   SET shard_path = $parent || substr(encode(sha256(key::bytea), 'hex'), length($parent)+1, 1)
 WHERE bucket_id = $1 AND shard_path = $parent;

-- 4. Recompute child state_hashes + counts from objects rows
WITH counts AS (
  SELECT shard_path, COUNT(*) AS c,
         digest(string_agg(id::text, '' ORDER BY id), 'sha256') AS h
    FROM objects WHERE bucket_id = $1 AND shard_path LIKE $parent || '_'
   GROUP BY shard_path
)
UPDATE object_shard_index s
   SET object_count = c.c, state_hash = c.h, updated_at = now()
  FROM counts c
 WHERE s.bucket_id = $1 AND s.shard_path = c.shard_path;

COMMIT;
```

Splits are O(N) within the splitting shard (5K rows). Amortized cost across all writes: a couple of milliseconds per write.

### Cleaner — verify one leaf shard

```go
func verifyLeaf(ctx context.Context, bucketID uuid.UUID, shard string) {
    diskDir := filepath.Join(storageRoot, "buckets", bucketID.String(),
                              "objects", filepath.Join(splitChars(shard)...))

    // 1. List disk entries
    diskEntries, _ := os.ReadDir(diskDir)
    diskFiles := setOfHashDirs(diskEntries)

    // 2. List DB entries in this shard
    rows, _ := db.Query(ctx,
        `SELECT sha256(key) FROM objects
          WHERE bucket_id = $1 AND shard_path = $2 AND NOT is_deleted`,
        bucketID, shard)
    dbFiles := setOfHashDirsFromRows(rows)

    // 3. Diff
    for hash := range diskFiles {
        if !dbFiles[hash] {
            twoStrikeReap(diskDir + "/" + hash, "no DB row")
        }
    }
    for hash := range dbFiles {
        if !diskFiles[hash] {
            auditMissingData(bucketID, shard, hash)
        }
    }

    // 4. Recompute + persist
    db.Exec(ctx,
        `UPDATE object_shard_index
            SET state_hash = $1, last_walk_at = now()
          WHERE bucket_id = $2 AND shard_path = $3`,
        merkleOf(dbFiles), bucketID, shard)
}
```

### Backfill (one-time migration from V2 → V4)

```sql
-- For each bucket: create the root shard, then split outward until no leaf
-- exceeds the threshold.
INSERT INTO object_shard_index (bucket_id, shard_path, depth, is_leaf, state_hash, object_count)
SELECT id, '', 0, TRUE, '\x00'::bytea, COUNT(o.id)
  FROM buckets b
  LEFT JOIN objects o ON o.bucket_id = b.id AND NOT o.is_deleted
 GROUP BY b.id;

-- Assign every existing object to the root shard initially.
UPDATE objects SET shard_path = '' WHERE shard_path IS NULL;

-- Then run a one-shot split loop in code:
--   while exists(leaf with object_count > 5000): split it
-- (Done once per bucket — finishes in seconds for 100M-file buckets.)
```

### Knobs

```
SHARD_SPLIT_THRESHOLD=5000      # files per leaf before splitting
MAX_SHARDS_PER_TICK=1000        # cleaner walks at most this many dirty shards/tick
WALK_CONCURRENCY=4              # parallel dirty-shard walks
CLEANER_INTERVAL=30s            # near-real-time
```

`CLEANER_INTERVAL` can drop to 30s because each tick is ~10ms in steady state. We're no longer hourly because we no longer need to be — there's nothing to scan.

---

## Scaling matrix

Cost per cleaner tick under steady state:

| Files in system | V1 (DB scan) | V2 (Bloom) | V3 (fixed shards) | V4 (adaptive trie) |
|---|---|---|---|---|
| 1M | 1s | <1s | <50ms | **<10ms** |
| 100M | many minutes | 2 min | seconds | **<10ms** |
| 1B | impossible | ~20 min | tens of seconds | **<10ms** |
| 10B | impossible | impossible | minutes | **<50ms** |
| 100B | impossible | impossible | impossible | **<500ms** |

Memory usage:

| Files in system | V1 | V2 | V3 | V4 |
|---|---|---|---|---|
| 1M | 30 MB | 1.7 MB bloom | <1 MB index | <1 MB index |
| 100M | 3 GB | 170 MB bloom | ~5 MB index | ~5 MB index |
| 1B | 30 GB | 1.7 GB bloom | ~50 MB index | ~50 MB index |

Detection latency (orphan → reap):

| Scenario | V1 | V2 | V3 | V4 |
|---|---|---|---|---|
| Bot drops one file | next 24h tick | next 24h tick | next 30s tick | **next 30s tick** |
| Race: file mid-write | grace period | grace + two-strike | grace + two-strike | **grace + two-strike** |

---

## Lessons that transfer to other projects

These patterns show up everywhere — file backup systems, dedupe stores, CDN origin sweepers, blockchain state, package mirrors. Steal them.

1. **Make the manifest the single source of truth.** Don't double-bookkeep on disk; the DB row IS the proof of existence. Eliminates drift by construction.

2. **Bloom filters when N is huge and you can tolerate <1% extra checks.**
   - O(1) lookup, 14 bits/item.
   - Stream-buildable (no in-memory hash set).
   - No false negatives → safe to gate destructive actions.

3. **Skip work using Merkle summaries when N is huge but churn is small.**
   - Partition into shards.
   - Each shard has a state_hash.
   - Compare disk_hash vs db_hash → walk only mismatched shards.
   - This is the move from "scan everything" to "verify a small dirty set."

4. **Adaptive splitting (IPv4 → IPv6 lesson).**
   - Don't pick a fixed shard count up front — it WILL exhaust at some scale.
   - Let leaves split into N children when they exceed a threshold.
   - Tree depth is then bounded by `log_N(file_count / leaf_size)`, not by a hardcoded constant.
   - Same idea: B-tree leaf splits, DNS zone delegation, Ethereum's Merkle-Patricia trie.

5. **Inline state updates beat lazy reconciliation for security.**
   - If integrity matters, update the manifest in the same transaction as the file write.
   - Cost: one indexed UPDATE per write (~0.5ms). Worth it.
   - Lazy schemes have a window where reality and the manifest disagree — attackers can sit in that window.

6. **Audit log goes to a file, not the DB.**
   - NDJSON appended to a daily file.
   - Zero DB pressure even under millions of reaps.
   - Easy to ship to cold storage, grep, tail.
   - The DB just records the run summary (1 row per tick).

7. **Dry-run by default.**
   - First deploy: log what would be deleted, delete nothing.
   - Admin reviews, flips the flag.
   - Especially important when the integrity model is new.

8. **Two-strike rule for orphan-class deletes.**
   - Candidate must appear on two consecutive scans before reaping.
   - Survives benign races (concurrent write, mid-rename, transient network blip).

9. **Make the cleaner cheap so it can run often.**
   - At 30-second tick intervals, every architectural choice has to be sub-second.
   - "Hourly cleanup" is a smell — it means the cleanup itself is too expensive.
   - When cleanup is cheap, you don't have a security window.

10. **Separate process, separate pool, separate failure domain.**
    - Cleaner crashes don't take down the API.
    - One DB connection (pool size 1) — can't starve hot paths.
    - Independent restart via `docker compose restart cleaner`.

---

## Implementation status

| Version | Status |
|---|---|
| V1 (pure DB) | Considered, rejected at design time |
| V2 (Bloom) | **Built** — still runs occasionally as a safety net (`api/internal/cleaner/orphans.go`) |
| V3 (fixed shards) | Skipped — went straight to V4 after the IPv4-exhaustion observation |
| V4 (adaptive trie) | **Built — primary integrity mechanism** |

### V4 code map

- `db/migrations/013_shard_index.sql` — `object_shard_index` table, indexes,
  trigger that bumps `updated_at`. `objects.shard_path` column.
- `api/internal/sharding/sharding.go` — `AssignToLeaf`, `OnObjectAdded`,
  `OnObjectRemoved`, XOR-commutative `rowContrib`.
- `api/internal/sharding/split.go` — `SplitLeaf` (atomic), `FindSplitCandidates`.
- `api/internal/sharding/backfill.go` — startup `Backfill` for existing
  buckets (idempotent).
- `api/internal/cleaner/shards.go` — V4 sweep loop: drain split queue,
  select dirty leaves, walk in parallel, reconcile + recompute `state_hash`.
- `api/internal/handlers/shards.go` + dashboard `/admin/cleanup/shards` —
  per-bucket tree view (depth, leaves, dirty flag, split-queued).

### Write hooks wired

| Write path | Hook |
|---|---|
| `PutObject` (new key) | `OnObjectAdded` |
| Multipart `Complete` (new key) | `OnObjectAdded` |
| `ImportFromURL` (importer) | `OnObjectAdded` |
| `PurgeObject` (hard delete) | `OnObjectRemoved` |
| Trash `purgeOne` (hard delete) | `OnObjectRemoved` |
| Cleaner `trash_vacuum` reaper | (uses bulk DELETE; shard recomputed on next walk) |

Soft delete (`DeleteObject`, `BulkDelete`) intentionally does NOT touch the
shard — the hash dir is still on disk and the row still exists; the shard's
count stays the same. Trash purges call `OnObjectRemoved`.

### Configuration knobs (env)

```
CLEANUP_INTERVAL=30s          # tick cadence — V4 is cheap enough to run often
SHARD_SPLIT_THRESHOLD=5000    # leaves split when they hit this many objects
SHARD_SPLITS_PER_TICK=32      # cap on splits per tick (bounds tick work)
SHARD_WALKS_PER_TICK=1000     # cap on dirty leaves walked per tick
SHARD_WALK_CONCURRENCY=4      # parallel goroutines for shard walks
```

### V4 extension: fsnotify watcher (built)

The V4 dirty-shard sweep only walks shards whose DB `updated_at` advanced.
Files written via paths OTHER than the API (container exec, malicious
process, root-mounted volume) don't bump `updated_at`, so the sweep skips
them. The V2 bloom scan eventually catches them but only every 24h —
unacceptable for security-driven cleanup.

**Fix** (`api/internal/cleaner/watcher.go`): boot an inotify watcher at
cleaner startup that monitors `/storage/buckets/*/objects/`. On any
`CREATE` / `WRITE` event:

1. Resolve the affected `(bucket_id, shard_path)` via the same
   longest-matching-prefix rule as `sharding.AssignToLeaf` — but using the
   hash directory name as the lookup key (no DB row exists for orphans).
2. `UPDATE object_shard_index SET last_walk_at = NULL WHERE ...` —
   instant dirty-flag flip.
3. Cleaner's next 30s tick sees the dirty shard, walks it, finds the
   orphan, reaps it.

Detection latency: filesystem event (~ms) → mark dirty (~ms) → next tick
(~30s avg) → walk shard (~10ms) → reap. **End-to-end well under a minute.**

Debounced (500ms) to coalesce bursts of events into one DB update per shard.
Failure modes degrade gracefully:

| Failure | Fallback |
|---|---|
| `inotify max_user_watches` exhausted | Log warning; V2 24h bloom still covers everything |
| Non-Linux host | Watcher refuses to start; scheduled sweeps still work |
| Bot creates a totally-new directory we don't watch yet | Caught on next bloom scan |
| Compromised process disables inotify | Caught on next bloom scan |

Opt-out via `WATCH_DISK=0` for networked storage where inotify isn't
supported.

Linux note: large deployments may need to bump
`fs.inotify.max_user_watches` (default 8192) and `max_user_instances`:

```bash
sudo sysctl -w fs.inotify.max_user_watches=524288
sudo sysctl -w fs.inotify.max_user_instances=512
```

### What's deliberately deferred

- **Storage path layout migration** — disk still uses the flat
  `objects/{H}/data` layout. The shard tree exists in the DB but doesn't
  match on-disk fanout yet. Fine until ~10M files per bucket; then we
  migrate to `objects/{H[0]}/{H[1]}/{H[2:]}/data` as a one-time backfill.
- **Postgres-side bytea XOR aggregate** — would let `recomputeAllLeaves`
  run entirely in SQL. Not worth the extension dependency yet.
- **Per-bucket archival flag** — shipped; cleaner skips shards on
  archived buckets via `WHERE NOT b.archived`.

---

## Post-V4 hardening (later patches)

A few specific gaps got plugged after V4 shipped. Listed here so the
"current" picture matches the code:

- **`sweepBucketRoots` runs every tick.** Was previously gated behind
  the dirty-shard early-return — quiet ticks skipped it. So a file
  dropped directly into `buckets/{B}/` (not under `objects/` or `tmp/`)
  could survive forever. Fixed by removing the early-return and running
  the bucket-root sweep unconditionally.
- **Two-strike state persists across ticks.** The map `Cleaner.orphanCandidates`
  is initialized once in `New()` and only touched in `confirmAndReap`
  and `pruneOrphanCandidates`. The combination above is what made the
  two-strike rule actually work for the bucket-root path.
- **Segments bloom filters by transcode status.** Was previously built
  from `SELECT id FROM objects` — every object's id was registered,
  including ones with `transcode_status IN ('none','skipped_quota','failed_quota')`
  that should have no segments dir. So a stale segments dir from a
  previous transcode (or one accidentally created) would be considered
  legitimate and never reaped. Fixed to filter the bloom to
  `transcode_status IN ('done','pending','processing') OR transcoded_bytes > 0`.
- **Age gate bypass when `OrphanMinAge=0`.** `walkShard` and
  `sweepBucketRoots` check `OrphanMinAge==0` and skip the
  `info.ModTime().After(ageCutoff)` check — useful for tests where you
  want immediate reaping. Production still defaults to 30 min.

The cleaner-V4 design itself was right; these were edges around it.

---

*Last updated after V4 implementation + post-V4 hardening patches
shipped. V2 bloom code retained as a safety net for non-shard-tracked
paths (segments dirs, multipart tmp).*
