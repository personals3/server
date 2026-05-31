package sharding

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SplitLeaf promotes one leaf into an internal node and creates 16 child
// leaves underneath. Then reassigns every object that pointed at the old
// leaf to its new child based on the next hash nibble.
//
// Atomic per leaf (single transaction). Splits cap at MaxDepth.
//
// Returns nil even if the leaf is already split (race-tolerant).
func SplitLeaf(ctx context.Context, pool *pgxpool.Pool, bucketID uuid.UUID, parent string) error {
	if len(parent) >= MaxDepth {
		return nil // address space exhausted at this branch
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin split tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the parent row so concurrent splitters serialize.
	var isLeaf bool
	if err := tx.QueryRow(ctx, `
		SELECT is_leaf FROM object_shard_index
		 WHERE bucket_id = $1 AND shard_path = $2
		 FOR UPDATE`, bucketID, parent,
	).Scan(&isLeaf); err != nil {
		if err == pgx.ErrNoRows {
			return nil // someone else already split or cleared it
		}
		return fmt.Errorf("lock parent: %w", err)
	}
	if !isLeaf {
		return nil // already split
	}

	// 1. Demote parent to internal node. Zero its counters — they live on the
	//    children now.
	if _, err := tx.Exec(ctx, `
		UPDATE object_shard_index
		   SET is_leaf      = FALSE,
		       state_hash   = '\x00'::bytea,
		       object_count = 0
		 WHERE bucket_id = $1 AND shard_path = $2`,
		bucketID, parent,
	); err != nil {
		return fmt.Errorf("demote parent: %w", err)
	}

	// 2. Create 16 child leaves. INSERT ON CONFLICT lets us re-run if a
	//    crash interrupted us mid-split — duplicates are harmless.
	const hex = "0123456789abcdef"
	for i := 0; i < 16; i++ {
		child := parent + string(hex[i])
		if _, err := tx.Exec(ctx, `
			INSERT INTO object_shard_index
			  (bucket_id, shard_path, depth, is_leaf, state_hash, object_count)
			VALUES ($1, $2, $3, TRUE, '\x00'::bytea, 0)
			ON CONFLICT (bucket_id, shard_path) DO NOTHING`,
			bucketID, child, len(child),
		); err != nil {
			return fmt.Errorf("create child %s: %w", child, err)
		}
	}

	// 3. Reassign objects. The nibble we care about is the one at position
	//    len(parent) of sha256(key).
	rows, err := tx.Query(ctx, `
		SELECT id, key FROM objects
		 WHERE bucket_id = $1 AND shard_path = $2`,
		bucketID, parent)
	if err != nil {
		return fmt.Errorf("list parent objects: %w", err)
	}

	type entry struct {
		ID  uuid.UUID
		Key string
	}
	var members []entry
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.ID, &e.Key); err != nil {
			rows.Close()
			return fmt.Errorf("scan parent member: %w", err)
		}
		members = append(members, e)
	}
	rows.Close()

	// Tally per-child state + count. Doing it in memory then flushing in
	// 16 small UPDATEs is way cheaper than 5K individual UPDATEs.
	type childState struct {
		Hash  []byte
		Count int
	}
	children := make(map[string]*childState, 16)
	for _, e := range members {
		childPath := parent + string(nibbleAt(e.Key, len(parent)))
		cs, ok := children[childPath]
		if !ok {
			cs = &childState{Hash: make([]byte, 32)}
			children[childPath] = cs
		}
		cs.Hash = xorInto(cs.Hash, rowContrib(e.ID, e.Key))
		cs.Count++
		if _, err := tx.Exec(ctx,
			`UPDATE objects SET shard_path = $1 WHERE id = $2`,
			childPath, e.ID,
		); err != nil {
			return fmt.Errorf("reassign object %s: %w", e.ID, err)
		}
	}

	for childPath, cs := range children {
		if _, err := tx.Exec(ctx, `
			UPDATE object_shard_index
			   SET state_hash   = $1,
			       object_count = $2
			 WHERE bucket_id = $3 AND shard_path = $4`,
			cs.Hash, cs.Count, bucketID, childPath,
		); err != nil {
			return fmt.Errorf("write child %s state: %w", childPath, err)
		}
	}

	return tx.Commit(ctx)
}

// FindSplitCandidates returns up to limit leaves currently over threshold.
// Cleaner drains this list on each tick.
func FindSplitCandidates(ctx context.Context, pool *pgxpool.Pool, threshold, limit int) ([]Candidate, error) {
	rows, err := pool.Query(ctx, `
		SELECT bucket_id, shard_path
		  FROM object_shard_index
		 WHERE is_leaf = TRUE
		   AND object_count > $1
		   AND depth < $2
		 ORDER BY object_count DESC
		 LIMIT $3`,
		threshold, MaxDepth, limit)
	if err != nil {
		return nil, fmt.Errorf("find candidates: %w", err)
	}
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.BucketID, &c.ShardPath); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Candidate identifies one leaf that should be split.
type Candidate struct {
	BucketID  uuid.UUID
	ShardPath string
}
