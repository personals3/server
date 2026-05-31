package handlers

import (
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/httpx"
)

// ShardHandler exposes per-bucket shard-tree visibility to the admin
// dashboard. Mounted under /admin/buckets/{name}/shards.
type ShardHandler struct {
	DB *pgxpool.Pool
}

type shardNodeDTO struct {
	ShardPath   string     `json:"shardPath"`
	Depth       int        `json:"depth"`
	IsLeaf      bool       `json:"isLeaf"`
	ObjectCount int        `json:"objectCount"`
	Dirty       bool       `json:"dirty"`
	LastWalkAt  *time.Time `json:"lastWalkAt,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// GET /admin/buckets/{name}/shards
// Returns the full shard tree for one bucket — leaves + internal nodes,
// ordered by shard_path so a UI can render it as an indented tree.
//
// Cheap: one indexed SELECT against object_shard_index (typically 1-thousands
// of rows even at 100M files).
func (h *ShardHandler) Tree(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "name")

	var bucketID uuid.UUID
	var objectCount int
	if err := h.DB.QueryRow(r.Context(), `
		SELECT b.id,
		       (SELECT COUNT(*) FROM objects o
		         WHERE o.bucket_id = b.id AND NOT o.is_deleted)
		  FROM buckets b
		 WHERE b.name = $1`, bucketName,
	).Scan(&bucketID, &objectCount); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "NO_BUCKET", "bucket not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	rows, err := h.DB.Query(r.Context(), `
		SELECT shard_path, depth, is_leaf, object_count,
		       (last_walk_at IS NULL OR last_walk_at < updated_at) AS dirty,
		       last_walk_at, updated_at
		  FROM object_shard_index
		 WHERE bucket_id = $1
		 ORDER BY shard_path`, bucketID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	tree := []shardNodeDTO{}
	leafCount, dirtyCount := 0, 0
	for rows.Next() {
		var n shardNodeDTO
		var lastWalk *time.Time
		if err := rows.Scan(&n.ShardPath, &n.Depth, &n.IsLeaf, &n.ObjectCount,
			&n.Dirty, &lastWalk, &n.UpdatedAt); err != nil {
			continue
		}
		n.LastWalkAt = lastWalk
		tree = append(tree, n)
		if n.IsLeaf {
			leafCount++
		}
		if n.Dirty {
			dirtyCount++
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"bucket":      bucketName,
		"bucketId":    bucketID,
		"objectCount": objectCount,
		"summary": map[string]any{
			"nodes":      len(tree),
			"leaves":     leafCount,
			"dirtyLeaves": dirtyCount,
		},
		"tree": tree,
	})
}
