// Package sharding maintains the per-bucket Merkle-trie of shards described
// in docs/come-up-designs/cleaner.md (V4).
//
// Two things live in this package:
//
//   1. Hot-path helpers — AssignToLeaf, OnObjectAdded, OnObjectRemoved.
//      Called from write handlers (PutObject, DeleteObject, etc) inside the
//      same transaction so the shard index stays consistent with objects.
//
//   2. Backfill + Split — used by the cleaner / startup migration to either
//      initialize the trie for an existing bucket or to grow it when leaves
//      get too big.
//
// State hash:
//
//   We use a commutative XOR-of-SHA256-fragments. For each object the per-
//   row contribution is sha256(object_id || '|' || key). The shard's hash is
//   the XOR of every contribution. Adding/removing an object is a single
//   XOR — order-independent, idempotent, and lets us update incrementally
//   without re-hashing the whole shard. Same trick PostgreSQL hash-join uses
//   for parallel aggregates.
package sharding

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// SplitThreshold is the per-leaf object_count at which the cleaner will
// split that leaf into 16 children. Tunable via env elsewhere; this is the
// default the helpers assume when computing growth decisions.
const SplitThreshold = 5000

// MaxDepth is the hard ceiling on shard_path length. 16 nibbles = 64 bits
// of address space (~1.8e19 unique shards). Trying to grow past this just
// keeps the existing leaf as-is.
const MaxDepth = 16

// HashKey returns the sha256 hex of an object key — same function the
// on-disk layout uses. Centralized here so V3/V4 storage paths stay in
// sync if we change the hash later.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// rowContrib returns the 32-byte XOR contribution one object makes to its
// shard's state_hash. Stable across cleaner runs.
func rowContrib(objectID uuid.UUID, key string) []byte {
	h := sha256.New()
	h.Write([]byte(objectID.String()))
	h.Write([]byte("|"))
	h.Write([]byte(key))
	return h.Sum(nil)
}

// xorInto returns dst XOR src as a new 32-byte slice. Caller-friendly when
// the destination came from a DB row (pgx returns []byte that we don't
// want to mutate in place).
func xorInto(dst, src []byte) []byte {
	out := make([]byte, len(dst))
	for i := range dst {
		out[i] = dst[i] ^ src[i]
	}
	return out
}

// dbConn is the minimal interface we need against pgx — both *pgxpool.Pool
// and pgx.Tx satisfy it, so the helpers work both inside an existing
// transaction (preferred) and against a bare pool.
//
// CommandTag lives in pgconn (NOT the top-level pgx package). Subtle, but
// pgxpool.Pool.Exec() really does return pgconn.CommandTag.
type dbConn interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// AssignToLeaf finds the leaf shard a given key belongs to and returns its
// shard_path. Uses the longest-matching-prefix rule: walk down from depth=0
// looking for the deepest is_leaf=TRUE row whose shard_path is a prefix of
// the key's hash.
//
// If the bucket has no shard rows yet (fresh bucket), this returns "" and
// the caller should ensure the root row gets created (OnObjectAdded does so).
func AssignToLeaf(ctx context.Context, db dbConn, bucketID uuid.UUID, key string) (string, error) {
	hash := HashKey(key)
	// Take the longest is_leaf row whose path is a prefix of `hash`. The
	// LIKE expression rewrites to an index range scan on the partial
	// idx_shard_lookup index.
	var shardPath string
	err := db.QueryRow(ctx, `
		SELECT shard_path
		  FROM object_shard_index
		 WHERE bucket_id = $1
		   AND is_leaf   = TRUE
		   AND $2::text LIKE shard_path || '%'
		 ORDER BY depth DESC
		 LIMIT 1`, bucketID, hash,
	).Scan(&shardPath)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", nil // bucket has no shard rows yet
		}
		return "", fmt.Errorf("assign leaf: %w", err)
	}
	return shardPath, nil
}

// OnObjectAdded is the per-write hook called from PutObject / multipart
// Complete / version restore / importer / etc. Three round trips:
//
//   1. AssignToLeaf  — find the right shard (SELECT)
//   2. Bump shard's object_count + updated_at (UPDATE)
//   3. Stamp objects.shard_path (UPDATE)
//
// We do NOT incrementally update state_hash here. The cleaner recomputes it
// on its next walk of the (now-dirty) shard. Reasons:
//
//   - One fewer round trip on the hot path (no SELECT then UPDATE on state_hash).
//   - state_hash inconsistency is self-healing: dirty shard → next tick walks
//     it → recompute → match. The cleaner is the source of truth, not the
//     write path.
//   - Multiple concurrent writes to the same shard would otherwise race on
//     the read-modify-write of state_hash; only the count bump is naturally
//     atomic via SQL.
//
// updated_at gets bumped by the trg_shard_index_updated trigger on any UPDATE,
// which is what makes the shard "dirty" for the next cleaner pass.
func OnObjectAdded(ctx context.Context, db dbConn, bucketID, objectID uuid.UUID, key string) error {
	shardPath, err := AssignToLeaf(ctx, db, bucketID, key)
	if err != nil {
		return err
	}
	if shardPath == "" {
		// Ensure the root row exists for this bucket (first-ever object).
		if _, err := db.Exec(ctx, `
			INSERT INTO object_shard_index
			  (bucket_id, shard_path, depth, is_leaf, state_hash, object_count)
			VALUES ($1, '', 0, TRUE, '\x00'::bytea, 0)
			ON CONFLICT (bucket_id, shard_path) DO NOTHING`,
			bucketID,
		); err != nil {
			return fmt.Errorf("ensure root shard: %w", err)
		}
	}

	if _, err := db.Exec(ctx, `
		UPDATE object_shard_index
		   SET object_count = object_count + 1
		 WHERE bucket_id = $1 AND shard_path = $2`,
		bucketID, shardPath,
	); err != nil {
		return fmt.Errorf("bump shard: %w", err)
	}

	if _, err := db.Exec(ctx, `
		UPDATE objects SET shard_path = $1 WHERE id = $2`,
		shardPath, objectID,
	); err != nil {
		return fmt.Errorf("stamp object.shard_path: %w", err)
	}
	return nil
}

// OnObjectRemoved is the per-hard-delete hook (PurgeObject, trash-vacuum
// reaper). Mirrors OnObjectAdded — bumps the dirty flag and decrements the
// count; state_hash gets reconciled on the next cleaner walk.
//
// Soft delete does NOT call this. The hash dir is still on disk and the row
// still exists; the shard's count stays the same.
func OnObjectRemoved(ctx context.Context, db dbConn, bucketID uuid.UUID, shardPath string) error {
	if shardPath == "" {
		// Object inserted before V4 backfill ran — its shard_path was NULL.
		// Backfill will fix this on its next run.
		return nil
	}
	if _, err := db.Exec(ctx, `
		UPDATE object_shard_index
		   SET object_count = GREATEST(object_count - 1, 0)
		 WHERE bucket_id = $1 AND shard_path = $2`,
		bucketID, shardPath,
	); err != nil {
		return fmt.Errorf("drop shard contribution: %w", err)
	}
	return nil
}

// nibbleAt returns the lowercased hex char at position i of the key's hash.
// Used during splits to decide which child a key maps to.
func nibbleAt(key string, i int) byte {
	h := HashKey(key)
	if i >= len(h) {
		return '0'
	}
	c := h[i]
	if c >= 'A' && c <= 'F' {
		c += 'a' - 'A'
	}
	return c
}
