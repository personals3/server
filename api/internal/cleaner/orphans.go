package cleaner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
	"github.com/google/uuid"
)

// Orphan scan — enforces the invariant:
//
//   Every file or directory under /storage/ must be reachable from a DB row.
//   If it isn't, it's an orphan and gets reaped.
//
// Three on-disk locations we walk:
//   /storage/buckets/{B}/objects/{H}/data         -> objects(bucket_id=B, sha256(key)=H)
//   /storage/buckets/{B}/objects/{H}/versions/{V} -> object_versions(version_id=V)
//   /storage/buckets/{B}/objects/{H}/data.tmp     -> in-flight write (mtime gate)
//   /storage/buckets/{B}/tmp/{U}                  -> multipart_uploads(upload_id=U)
//   /storage/segments/{O}                         -> objects(id=O)
//   /storage/buckets/{B}                          -> buckets(id=B)
//
// Membership is checked against a bloom filter rebuilt every BloomRebuildInterval.
// Bloom never has false negatives — "not in filter" → "definitely not in DB" →
// safe to delete. False positives (~0.1%) just survive this round.
//
// Two safety rails before any deletion:
//   1. Age gate: file mtime must be older than max(OrphanMinAge, bloom-age).
//      Closes the "file created after bloom was built" race without needing
//      a Valkey delta set.
//   2. Two-strike rule: a path must show up as a candidate on TWO consecutive
//      scans before we delete. Survives transient races (e.g. a brief moment
//      where the DB row exists but disk write is mid-flight).
//
// All of this is dry-run capable — the candidate is always logged; only the
// actual delete is gated by cfg.DryRun.

type bloomState struct {
	hashes   *bloom.BloomFilter // bucket_id + "|" + hash
	versions *bloom.BloomFilter // object_id + "|" + version_id
	uploads  *bloom.BloomFilter // bucket_id + "|" + upload_id
	segments *bloom.BloomFilter // object_id (the segments dir name)
	buckets  *bloom.BloomFilter // bucket_id
	builtAt  time.Time
}

func (c *Cleaner) reapOrphans(ctx context.Context, rec *runRecord) error {
	bs, err := c.buildBloomFilters(ctx)
	if err != nil {
		return fmt.Errorf("build bloom: %w", err)
	}

	// Anything younger than this is left alone. Closes the "created after bloom"
	// race window without needing a Valkey delta set.
	minAge := c.cfg.OrphanMinAge
	if elapsed := time.Since(bs.builtAt); elapsed > minAge {
		minAge = elapsed
	}
	ageCutoff := time.Now().Add(-minAge)

	// 1. Walk /storage/segments/{O}
	segRoot := filepath.Join(c.cfg.StorageRoot, "segments")
	c.walkOneLevel(rec, segRoot, ageCutoff, func(name, path string) bool {
		if !isUUIDName(name) {
			return false // odd; leave it
		}
		return bs.segments.TestString(name)
	}, "orphan_segments", "segment_dir")

	// 2. Walk /storage/buckets/{B}
	bktRoot := filepath.Join(c.cfg.StorageRoot, "buckets")
	c.walkOneLevel(rec, bktRoot, ageCutoff, func(name, path string) bool {
		if !isUUIDName(name) {
			return false
		}
		return bs.buckets.TestString(name)
	}, "orphan_bucket_dir", "bucket_dir")

	// 3. For each EXISTING bucket dir, walk objects/{H} and tmp/{U}
	bucketEntries, _ := os.ReadDir(bktRoot)
	for _, be := range bucketEntries {
		if !be.IsDir() || !isUUIDName(be.Name()) {
			continue
		}
		bucketID := be.Name()
		if !bs.buckets.TestString(bucketID) {
			continue // outer scan handles this
		}

		// 3a. objects/{H}
		objectsDir := filepath.Join(bktRoot, bucketID, "objects")
		c.walkOneLevel(rec, objectsDir, ageCutoff, func(name, path string) bool {
			return bs.hashes.TestString(bucketID + "|" + name)
		}, "orphan_hash_dir", "hash_dir")

		// 3b. objects/{H}/versions/{V} — only valid hash dirs walked here
		hashEntries, _ := os.ReadDir(objectsDir)
		for _, he := range hashEntries {
			if !he.IsDir() {
				continue
			}
			if !bs.hashes.TestString(bucketID + "|" + he.Name()) {
				continue // outer scan handles it
			}
			versionsDir := filepath.Join(objectsDir, he.Name(), "versions")
			c.walkOneLevel(rec, versionsDir, ageCutoff, func(vid, path string) bool {
				// We don't know object_id without a DB lookup, so the bloom
				// is keyed on just the version_id. Acceptable false-positive
				// risk because version_ids are random UUID hex (32 chars).
				return bs.versions.TestString(vid)
			}, "orphan_version", "version_file")

			// 3c. data.tmp older than min-age — process died mid-write
			tmpFile := filepath.Join(objectsDir, he.Name(), "data.tmp")
			if fi, err := os.Stat(tmpFile); err == nil && fi.ModTime().Before(ageCutoff) {
				c.confirmAndReap(rec, "orphan_tmp_write", "tmp_file", tmpFile,
					"abandoned mid-upload file — older than the safety window, so the writing process likely crashed")
			}
		}

		// 3d. tmp/{U}
		tmpDir := filepath.Join(bktRoot, bucketID, "tmp")
		c.walkOneLevel(rec, tmpDir, ageCutoff, func(uploadID, path string) bool {
			return bs.uploads.TestString(bucketID + "|" + uploadID)
		}, "orphan_upload", "upload_dir")
	}

	// 4. Prune the in-memory two-strike map so it doesn't grow forever.
	c.pruneOrphanCandidates()

	return nil
}

// walkOneLevel iterates the entries directly under dir, applies isAlive,
// and routes survivors through the two-strike + dry-run gating in
// confirmAndReap.
func (c *Cleaner) walkOneLevel(rec *runRecord, dir string, ageCutoff time.Time,
	isAlive func(name, path string) bool,
	action, targetType string,
) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			rec.Errors = append(rec.Errors,
				fmt.Sprintf("walk %s: %v", dir, err))
		}
		return
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().After(ageCutoff) {
			continue // too young, grace period
		}
		if isAlive(e.Name(), path) {
			continue
		}
		c.confirmAndReap(rec, action, targetType, path,
			"on disk but no matching database row")
	}
}

// confirmAndReap applies the two-strike rule then logs (and deletes in
// non-dry-run mode) the orphan.
func (c *Cleaner) confirmAndReap(rec *runRecord, action, targetType, path, reason string) {
	if c.cfg.OrphanTwoStrike {
		if _, prev := c.orphanCandidates[path]; !prev {
			c.orphanCandidates[path] = struct{}{}
			// First strike — log as "candidate" so it's visible in the audit
			// trail but no delete yet.
			rec.audit("orphan_candidate", targetType, path,
				reason+" — flagged this tick; will remove on the next tick if still present (two-strike safety rule)", 0)
			return
		}
		// Second strike — confirmed; remove from the candidate set and reap.
		delete(c.orphanCandidates, path)
	}
	c.removePathSafely(rec, action, targetType, path, reason)
}

// pruneOrphanCandidates removes paths from the two-strike set that still
// exist on disk but were re-confirmed (handled) OR that have vanished
// (raced with another process). Keeps the map bounded.
func (c *Cleaner) pruneOrphanCandidates() {
	for path := range c.orphanCandidates {
		if _, err := os.Stat(path); err != nil {
			delete(c.orphanCandidates, path)
		}
	}
}

// buildBloomFilters streams the DB membership into in-memory bloom filters.
// One round trip per table; the rows are NOT held in memory.
func (c *Cleaner) buildBloomFilters(ctx context.Context) (*bloomState, error) {
	bs := &bloomState{builtAt: time.Now()}
	const fp = 0.001 // 0.1% false positive

	// buckets
	{
		var n int64
		_ = c.db.QueryRow(ctx, `SELECT COUNT(*) FROM buckets`).Scan(&n)
		bs.buckets = bloom.NewWithEstimates(uintMax(n, 1024), fp)
		rows, err := c.db.Query(ctx, `SELECT id::text FROM buckets`)
		if err != nil {
			return nil, fmt.Errorf("query buckets: %w", err)
		}
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			bs.buckets.AddString(id)
		}
		rows.Close()
	}

	// objects -> hashes + segments
	//
	// The hashes bloom must contain EVERY object (orphan hash dirs are about
	// disk vs DB existence).
	//
	// The segments bloom must ONLY contain objects that actually have
	// segments on disk (or are mid-transcode). Including every object — as
	// V1 of this code did — meant a /storage/segments/{id} directory was
	// considered legitimate even when the owning object had been marked
	// skipped_quota / failed_quota / never-transcoded. That left orphan
	// transcode output undeleted forever. Filtering here lets the regular
	// orphan_segments path reap them on the next sweep.
	{
		var n int64
		_ = c.db.QueryRow(ctx, `SELECT COUNT(*) FROM objects`).Scan(&n)
		bs.hashes = bloom.NewWithEstimates(uintMax(n, 1024), fp)
		bs.segments = bloom.NewWithEstimates(uintMax(n, 1024), fp)
		rows, err := c.db.Query(ctx, `
			SELECT id::text, bucket_id::text, key,
			       transcode_status,
			       COALESCE(transcoded_bytes, 0) AS trb
			  FROM objects`)
		if err != nil {
			return nil, fmt.Errorf("query objects: %w", err)
		}
		for rows.Next() {
			var id, bid, key, tstatus string
			var trb int64
			if err := rows.Scan(&id, &bid, &key, &tstatus, &trb); err != nil {
				continue
			}
			bs.hashes.AddString(bid + "|" + sha256Hex(key))
			// Only register a segments dir as "expected on disk" if the
			// object's transcode could legitimately have output present.
			if trb > 0 || tstatus == "done" ||
				tstatus == "pending" || tstatus == "processing" {
				bs.segments.AddString(id)
			}
		}
		rows.Close()
	}

	// object_versions
	{
		var n int64
		_ = c.db.QueryRow(ctx, `SELECT COUNT(*) FROM object_versions`).Scan(&n)
		bs.versions = bloom.NewWithEstimates(uintMax(n, 1024), fp)
		rows, err := c.db.Query(ctx, `SELECT version_id FROM object_versions`)
		if err != nil {
			return nil, fmt.Errorf("query object_versions: %w", err)
		}
		for rows.Next() {
			var vid string
			_ = rows.Scan(&vid)
			bs.versions.AddString(vid)
		}
		rows.Close()
	}

	// multipart_uploads
	{
		var n int64
		_ = c.db.QueryRow(ctx,
			`SELECT COUNT(*) FROM multipart_uploads WHERE status = 'in-progress'`).Scan(&n)
		bs.uploads = bloom.NewWithEstimates(uintMax(n, 256), fp)
		rows, err := c.db.Query(ctx,
			`SELECT bucket_id::text, upload_id FROM multipart_uploads WHERE status = 'in-progress'`)
		if err != nil {
			return nil, fmt.Errorf("query multipart: %w", err)
		}
		for rows.Next() {
			var bid, uid string
			_ = rows.Scan(&bid, &uid)
			bs.uploads.AddString(bid + "|" + uid)
		}
		rows.Close()
	}

	log.Printf("cleaner: bloom built — buckets=%d hashes=%d versions=%d uploads=%d segments=%d",
		bs.buckets.ApproximatedSize(), bs.hashes.ApproximatedSize(),
		bs.versions.ApproximatedSize(), bs.uploads.ApproximatedSize(),
		bs.segments.ApproximatedSize())
	return bs, nil
}

func uintMax(n int64, floor uint) uint {
	if n < int64(floor) {
		return floor
	}
	// add 25% headroom so the FP rate stays at the requested level under churn
	return uint(n) + uint(n)/4
}

func isUUIDName(s string) bool {
	_, err := uuid.Parse(s)
	return err == nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

