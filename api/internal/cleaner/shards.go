package cleaner

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/personals3/api/internal/sharding"
)

// V4 cleaner — dirty-shard reconciliation.
//
// Replaces the bloom-based orphan scan from V2. Each tick:
//
//   1. Drain split candidates (leaves that grew past SplitThreshold).
//   2. SELECT dirty leaves (last_walk_at < updated_at).
//   3. Walk each in parallel; reap orphans, log missing data files,
//      recompute state_hash, stamp last_walk_at.
//
// At steady state with no churn this loop touches ~zero files per tick.
// At 100M+ files only the small dirty set is scanned — milliseconds to
// seconds, never minutes.

const (
	// SplitsPerTick caps how many leaves are split in one cleaner pass.
	// Each split is O(leaf size) ~= O(SplitThreshold), so this bounds work.
	SplitsPerTick = 32
	// ShardsPerTick caps how many dirty leaves we walk in one pass.
	ShardsPerTick = 1000
	// WalkConcurrency parallelism for the per-shard reconcile loop.
	WalkConcurrency = 4
)

// runShardSweep is called from runOnce (after the time-based reapers).
// Returns nil on success; per-shard failures are recorded in rec but don't
// abort the whole tick.
func (c *Cleaner) runShardSweep(ctx context.Context, rec *runRecord) error {
	// 1) Process splits queued by hot-path writes.
	cands, err := sharding.FindSplitCandidates(ctx, c.db, sharding.SplitThreshold, SplitsPerTick)
	if err != nil {
		return fmt.Errorf("find splits: %w", err)
	}
	for _, cand := range cands {
		if err := sharding.SplitLeaf(ctx, c.db, cand.BucketID, cand.ShardPath); err != nil {
			rec.Errors = append(rec.Errors,
				fmt.Sprintf("split %s/%s: %v", cand.BucketID, cand.ShardPath, err))
			continue
		}
		rec.audit("shard_split", "shard",
			fmt.Sprintf("%s/%s", cand.BucketID, cand.ShardPath),
			"object_count > SplitThreshold", 0)
	}

	// 2) Find dirty leaves and walk them. Archived buckets are excluded so
	//    the cleaner doesn't keep re-walking cold-storage data on every tick.
	rows, err := c.db.Query(ctx, `
		SELECT b.id, b.name, s.shard_path
		  FROM object_shard_index s
		  JOIN buckets b ON b.id = s.bucket_id
		 WHERE s.is_leaf = TRUE
		   AND NOT b.archived
		   AND (s.last_walk_at IS NULL OR s.last_walk_at < s.updated_at)
		 ORDER BY s.updated_at
		 LIMIT $1`, ShardsPerTick)
	if err != nil {
		return fmt.Errorf("find dirty shards: %w", err)
	}
	defer rows.Close()

	type job struct {
		BucketID   uuid.UUID
		BucketName string
		ShardPath  string
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.BucketID, &j.BucketName, &j.ShardPath); err != nil {
			continue
		}
		jobs = append(jobs, j)
	}
	rows.Close()

	// 3) Walk in parallel. Each goroutine takes one shard at a time off the chan.
	if len(jobs) > 0 {
		jobCh := make(chan job)
		var wg sync.WaitGroup
		for i := 0; i < WalkConcurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := range jobCh {
					c.walkShard(ctx, rec, j.BucketID, j.BucketName, j.ShardPath)
				}
			}()
		}
		for _, j := range jobs {
			jobCh <- j
		}
		close(jobCh)
		wg.Wait()

		rec.audit("shard_sweep_summary", "tick", "",
			fmt.Sprintf("dirty=%d", len(jobs)), 0)
	}

	// 4) Bucket-root sweep — must run EVERY tick, even when no shards are
	//    dirty. The early-return above used to skip this on quiet ticks,
	//    which broke the two-strike rule for files dropped directly into
	//    buckets/{B}/ (NOT under objects/ or tmp/). The API never writes
	//    here, so anything that shows up is by definition unauthorized.
	if err := c.sweepBucketRoots(ctx, rec); err != nil {
		rec.Errors = append(rec.Errors,
			fmt.Sprintf("bucket_root_sweep: %v", err))
	}

	// 5) Multipart-tmp sweep — buckets/{B}/tmp/{upload_id} for any upload
	//    that's no longer in-progress. Was previously only covered by the
	//    24h orphan_scan path; this runs every tick so that aborted-but-
	//    leftover dirs (e.g. when Abort's os.RemoveAll silently fails)
	//    get reaped within a couple of cleaner intervals instead of a day.
	if err := c.sweepMultipartTmp(ctx, rec); err != nil {
		rec.Errors = append(rec.Errors,
			fmt.Sprintf("multipart_tmp_sweep: %v", err))
	}

	return nil
}

// sweepMultipartTmp checks each bucket's tmp/ directory for upload-id
// subdirs whose multipart_uploads row is missing, aborted, or completed.
// Anything matching gets the two-strike treatment.
//
// Cost: one ReadDir per bucket + one bulk query for in-progress uploads.
// On a healthy host this is microseconds; on a host with a stuck Abort
// it catches the leftover within ~60s.
func (c *Cleaner) sweepMultipartTmp(ctx context.Context, rec *runRecord) error {
	// Pull the set of in-progress upload_ids per bucket once, up front, so
	// we don't query the DB inside the file walk.
	rows, err := c.db.Query(ctx, `
		SELECT bucket_id::text, upload_id
		  FROM multipart_uploads
		 WHERE status = 'in-progress' AND expires_at > now()`)
	if err != nil {
		return fmt.Errorf("list in-progress uploads: %w", err)
	}
	inProgress := make(map[string]struct{}) // "bucketID|uploadID"
	for rows.Next() {
		var bid, uid string
		if err := rows.Scan(&bid, &uid); err == nil {
			inProgress[bid+"|"+uid] = struct{}{}
		}
	}
	rows.Close()

	brows, err := c.db.Query(ctx,
		`SELECT id, name FROM buckets WHERE NOT archived`)
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	defer brows.Close()

	ageCutoff := time.Now().Add(-c.cfg.OrphanMinAge)
	skipAgeGate := c.cfg.OrphanMinAge == 0
	for brows.Next() {
		var bid uuid.UUID
		var bname string
		if err := brows.Scan(&bid, &bname); err != nil {
			continue
		}
		tmpDir := filepath.Join(c.cfg.StorageRoot, "buckets", bid.String(), "tmp")
		entries, err := os.ReadDir(tmpDir)
		if err != nil {
			continue // no tmp dir → nothing to reap
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if _, alive := inProgress[bid.String()+"|"+name]; alive {
				continue // legitimate in-progress upload
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if !skipAgeGate && info.ModTime().After(ageCutoff) {
				continue
			}
			fullPath := filepath.Join(tmpDir, name)
			c.confirmAndReap(rec, "orphan_multipart_tmp", "upload_dir", fullPath,
				fmt.Sprintf("leftover multipart upload directory in %q — the upload either finished, was aborted, or expired", bname))
		}
	}
	return nil
}

// sweepBucketRoots checks each live bucket directory for unexpected
// top-level entries. Legitimate names are "objects" and "tmp"; anything
// else (file or dir) gets the orphan treatment.
//
// Runs on every shard-sweep tick. Cost: one ReadDir per bucket, capped at
// the bucket count which is tiny.
func (c *Cleaner) sweepBucketRoots(ctx context.Context, rec *runRecord) error {
	rows, err := c.db.Query(ctx, `
		SELECT id, name FROM buckets WHERE NOT archived`)
	if err != nil {
		return fmt.Errorf("list buckets: %w", err)
	}
	defer rows.Close()

	ageCutoff := time.Now().Add(-c.cfg.OrphanMinAge)
	skipAgeGate := c.cfg.OrphanMinAge == 0
	for rows.Next() {
		var bid uuid.UUID
		var bname string
		if err := rows.Scan(&bid, &bname); err != nil {
			continue
		}
		bucketDir := filepath.Join(c.cfg.StorageRoot, "buckets", bid.String())
		entries, err := os.ReadDir(bucketDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if name == "objects" || name == "tmp" {
				continue // expected layout
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if !skipAgeGate && info.ModTime().After(ageCutoff) {
				continue
			}
			fullPath := filepath.Join(bucketDir, name)
			c.confirmAndReap(rec, "orphan_in_bucket_root", "stray", fullPath,
				fmt.Sprintf("unexpected file %q dropped into bucket %q (the API never writes here)", name, bname))
		}
	}
	return nil
}

// isHexLower returns true if s is non-empty and consists of only [0-9a-f].
// Matches what sharding.HashKey produces (lowercase sha256 hex).
func isHexLower(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// walkShard reconciles one leaf shard against its on-disk hash directories.
//
//   diskHashes  = the set of {H} subdirs under buckets/{B}/objects/ that start
//                 with shard's prefix
//   dbHashes    = sha256(key) of every object in this shard (not is_deleted)
//
// Diff:
//   diskHashes - dbHashes = orphan hash dirs → reap (two-strike + dry-run guards)
//   dbHashes  - diskHashes = MISSING_DATA → log only (admin investigates)
//
// Then recompute the leaf's state_hash + stamp last_walk_at.
func (c *Cleaner) walkShard(ctx context.Context, rec *runRecord,
	bucketID uuid.UUID, bucketName, shardPath string) {

	bucketObjectsDir := filepath.Join(c.cfg.StorageRoot, "buckets", bucketID.String(), "objects")

	// 1) Scan disk for hash dirs that START WITH shardPath.
	//    The on-disk layout is currently flat (objects/{H}/data) — every hash
	//    dir is a direct child. We list once and filter in-memory.
	diskEntries, err := os.ReadDir(bucketObjectsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			rec.Errors = append(rec.Errors,
				fmt.Sprintf("readdir %s: %v", bucketObjectsDir, err))
		}
		return
	}

	// Build a set of hash names whose prefix matches this shard. Anything
	// that's NOT a 64-char hex dir under objects/ is unexpected — the API
	// only ever creates SHA256-hash dirs there. Reap immediately (subject
	// to the usual age + two-strike gates).
	diskHashes := make(map[string]os.FileInfo)
	// Age gate: skip entirely when MinAge=0 so brand-new orphans are
	// caught immediately (matches the user's aggressive test config).
	ageCutoffStray := time.Now().Add(-c.cfg.OrphanMinAge)
	skipAgeGate := c.cfg.OrphanMinAge == 0
	for _, e := range diskEntries {
		name := e.Name()
		if len(name) == 64 && isHexLower(name) {
			if shardPath != "" && len(name) >= len(shardPath) && name[:len(shardPath)] != shardPath {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			diskHashes[name] = info
			continue
		}
		// Stray entry under objects/ — not a hash dir. Could be a file
		// (`evil.sh`), a weirdly-named directory, anything.
		fullPath := filepath.Join(bucketObjectsDir, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !skipAgeGate && info.ModTime().After(ageCutoffStray) {
			continue
		}
		c.confirmAndReap(rec, "orphan_stray_in_objects", "stray", fullPath,
			fmt.Sprintf("unexpected file %q sitting in %q's objects/ folder (only sha256 hash directories belong here)", name, bucketName))
	}

	// 2) Load DB hashes for this shard.
	dbHashes := make(map[string]uuid.UUID)
	dbRows, err := c.db.Query(ctx, `
		SELECT id, key FROM objects
		 WHERE bucket_id = $1 AND shard_path = $2 AND NOT is_deleted`,
		bucketID, shardPath)
	if err != nil {
		rec.Errors = append(rec.Errors,
			fmt.Sprintf("list shard %s/%s: %v", bucketID, shardPath, err))
		return
	}
	dbList := []shardMember{}
	for dbRows.Next() {
		var r shardMember
		if err := dbRows.Scan(&r.ID, &r.Key); err != nil {
			continue
		}
		dbHashes[sha256Hex(r.Key)] = r.ID
		dbList = append(dbList, r)
	}
	dbRows.Close()

	// 3) Diff.
	ageCutoff := time.Now().Add(-c.cfg.OrphanMinAge)
	ageGatedCandidates := 0
	for hash, info := range diskHashes {
		if _, ok := dbHashes[hash]; ok {
			continue
		}
		// Orphan candidate. Age gate: too-young files get a pass this round
		// AND keep the shard "dirty" so we revisit on the next tick. Without
		// this, the orphan would never get reaped until something else
		// triggered a re-walk (or the 24h V2 bloom scan).
		if info.ModTime().After(ageCutoff) {
			ageGatedCandidates++
			continue
		}
		hashDir := filepath.Join(bucketObjectsDir, hash)
		c.confirmAndReap(rec, "orphan_hash_dir", "hash_dir", hashDir,
			fmt.Sprintf("object data on disk under %q but no matching row in the database (shard %s)", bucketName, shardPath))
	}
	for hash, id := range dbHashes {
		if _, ok := diskHashes[hash]; !ok {
			rec.audit("missing_data", "object", id.String(),
				fmt.Sprintf("database has this object in %q (shard %s) but its data file is missing — needs investigation",
					bucketName, shardPath), 0)
		}
	}

	// 4) Recompute state_hash. last_walk_at is left NULL if we deferred any
	//    candidates — the shard stays dirty so the next tick re-checks once
	//    the age gate has passed. Otherwise the shard is fully reconciled
	//    and we stamp last_walk_at = now() to mark it clean.
	newHash := computeShardHash(dbList)
	var sql string
	if ageGatedCandidates > 0 {
		sql = `UPDATE object_shard_index
		          SET state_hash   = $1,
		              object_count = $2,
		              last_walk_at = NULL          -- stay dirty for re-check
		        WHERE bucket_id = $3 AND shard_path = $4`
	} else {
		sql = `UPDATE object_shard_index
		          SET state_hash   = $1,
		              object_count = $2,
		              last_walk_at = now()
		        WHERE bucket_id = $3 AND shard_path = $4`
	}
	if _, err := c.db.Exec(ctx, sql, newHash, len(dbList), bucketID, shardPath); err != nil {
		rec.Errors = append(rec.Errors,
			fmt.Sprintf("update shard hash %s/%s: %v", bucketID, shardPath, err))
	}
	if ageGatedCandidates > 0 {
		rec.audit("orphan_deferred", "shard",
			fmt.Sprintf("%s/%s", bucketID, shardPath),
			fmt.Sprintf("%d possible leftovers in this shard are still within the safety window — will re-check after they age past ORPHAN_MIN_AGE_MINUTES",
				ageGatedCandidates), 0)
	}
}

// shardMember is one (object_id, key) pair within a shard. Used by
// computeShardHash and the walk loop.
type shardMember struct {
	ID  uuid.UUID
	Key string
}

// computeShardHash mirrors sharding.rowContrib + xorInto. Kept here as well
// because the cleaner package can't easily reach into sharding's unexported
// helpers — and keeping the algorithm explicit here doubles as docs.
func computeShardHash(rows []shardMember) []byte {
	out := make([]byte, 32)
	// Sort by id so different orderings produce the same hash even though
	// XOR is already commutative — keeps the diff deterministic for tests.
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID.String() < rows[j].ID.String() })
	for _, r := range rows {
		h := sha256.New()
		h.Write([]byte(r.ID.String()))
		h.Write([]byte("|"))
		h.Write([]byte(r.Key))
		c := h.Sum(nil)
		for i := range out {
			out[i] ^= c[i]
		}
	}
	return out
}

// Sanity-log on cleaner startup so admins can see V4 is engaged.
func init() {
	log.Printf("cleaner: V4 shard sweep enabled (concurrency=%d, splits/tick=%d, shards/tick=%d)",
		WalkConcurrency, SplitsPerTick, ShardsPerTick)
}
