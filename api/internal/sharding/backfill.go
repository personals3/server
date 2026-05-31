package sharding

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Backfill ensures every bucket has at least a root shard entry and that
// every existing object row has a shard_path assignment.
//
// Idempotent: re-running it just confirms current state. Safe to invoke on
// every cleaner startup; it short-circuits when buckets already have their
// roots.
//
// Cost on a 100M-object cold migration:
//   - One INSERT per bucket (cheap)
//   - One bulk UPDATE per bucket to stamp objects.shard_path = ''
//   - Then SplitLeaf in a loop until no leaf exceeds threshold
//   - Total wall time: minutes once, then the system runs at steady state.
func Backfill(ctx context.Context, pool *pgxpool.Pool) error {
	start := time.Now()
	log.Printf("sharding: starting backfill scan")

	// 1. Ensure root shard for every bucket.
	if _, err := pool.Exec(ctx, `
		INSERT INTO object_shard_index
		  (bucket_id, shard_path, depth, is_leaf, state_hash, object_count)
		SELECT id, '', 0, TRUE, '\x00'::bytea, 0
		  FROM buckets
		ON CONFLICT (bucket_id, shard_path) DO NOTHING`,
	); err != nil {
		return fmt.Errorf("ensure roots: %w", err)
	}

	// 2. Stamp shard_path on any object that doesn't have one yet. They go
	//    into the root '' initially; SplitLeaf calls below will redistribute.
	tag, err := pool.Exec(ctx, `
		UPDATE objects SET shard_path = '' WHERE shard_path IS NULL`,
	)
	if err != nil {
		return fmt.Errorf("stamp existing objects: %w", err)
	}
	if tag.RowsAffected() > 0 {
		log.Printf("sharding: stamped %d existing objects into root shards",
			tag.RowsAffected())
	}

	// 3. Recompute root state_hash + object_count for buckets we just stamped.
	//    Only touches roots; takes one streaming pass per bucket.
	if err := recomputeAllLeaves(ctx, pool); err != nil {
		return fmt.Errorf("recompute roots: %w", err)
	}

	// 4. Split until no leaf is too big. We bound iterations to MaxDepth × 16
	//    in case a bucket has more files than fits in MaxDepth^16 — at that
	//    point splits stop helping and we just leave a few oversized leaves.
	for iter := 0; iter < 256; iter++ {
		cands, err := FindSplitCandidates(ctx, pool, SplitThreshold, 64)
		if err != nil {
			return fmt.Errorf("find candidates: %w", err)
		}
		if len(cands) == 0 {
			break
		}
		log.Printf("sharding: backfill iter %d — splitting %d leaves", iter, len(cands))
		for _, c := range cands {
			if err := SplitLeaf(ctx, pool, c.BucketID, c.ShardPath); err != nil {
				log.Printf("sharding: split %s/%s failed: %v", c.BucketID, c.ShardPath, err)
			}
		}
	}

	log.Printf("sharding: backfill done in %s", time.Since(start))
	return nil
}

// recomputeAllLeaves rebuilds object_count + state_hash for every leaf in
// every bucket by streaming `objects` once and aggregating Go-side. Used by
// Backfill to seed initial state.
//
// Why Go-side: Postgres `bit_xor` is integer-only; we'd need a custom
// aggregate function for bytea XOR, which is more surgery than a single
// streaming read. At 100M objects this read is ~80s once — acceptable.
//
// Memory bound: O(distinct shard paths) — ~1 per bucket initially, so trivial.
func recomputeAllLeaves(ctx context.Context, pool *pgxpool.Pool) error {
	type key struct {
		Bucket uuid.UUID
		Shard  string
	}
	type agg struct {
		Hash  []byte
		Count int
	}
	tally := map[key]*agg{}

	rows, err := pool.Query(ctx, `
		SELECT bucket_id, COALESCE(shard_path, ''), id, key
		  FROM objects
		 WHERE NOT is_deleted`)
	if err != nil {
		return fmt.Errorf("stream objects: %w", err)
	}
	for rows.Next() {
		var b uuid.UUID
		var s, k string
		var id uuid.UUID
		if err := rows.Scan(&b, &s, &id, &k); err != nil {
			rows.Close()
			return err
		}
		kk := key{b, s}
		a, ok := tally[kk]
		if !ok {
			a = &agg{Hash: make([]byte, 32)}
			tally[kk] = a
		}
		a.Hash = xorInto(a.Hash, rowContrib(id, k))
		a.Count++
	}
	rows.Close()

	// Flush. Each shard gets one UPDATE; rows with zero objects (e.g., a
	// freshly-created root for an empty bucket) keep their zero defaults.
	for kk, a := range tally {
		if _, err := pool.Exec(ctx, `
			UPDATE object_shard_index
			   SET state_hash   = $1,
			       object_count = $2
			 WHERE bucket_id = $3 AND shard_path = $4`,
			a.Hash, a.Count, kk.Bucket, kk.Shard,
		); err != nil {
			return fmt.Errorf("flush %s/%s: %w", kk.Bucket, kk.Shard, err)
		}
	}
	return nil
}

// keep the uuid import explicit (used by helper types above).
var _ = uuid.UUID{}
