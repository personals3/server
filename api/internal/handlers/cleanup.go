package handlers

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/httpx"
)

// CleanupHandler exposes the cleaner's runs + per-run NDJSON event tails to
// the admin dashboard. Mounted under /admin/cleanup.
//
// The cleaner itself runs in its own container — this handler only reads.
// To trigger an ad-hoc run, the admin endpoint just bumps a Valkey key the
// cleaner could poll OR sends SIGUSR1 to the cleaner container. We use a
// Postgres NOTIFY instead so no extra wiring is needed in the cleaner.
type CleanupHandler struct {
	DB          *pgxpool.Pool
	StorageRoot string
}

type cleanupRunDTO struct {
	ID           uuid.UUID      `json:"id"`
	StartedAt    time.Time      `json:"startedAt"`
	FinishedAt   *time.Time     `json:"finishedAt,omitempty"`
	DurationMS   int64          `json:"durationMs"`
	DryRun       bool           `json:"dryRun"`
	BytesFreed   int64          `json:"bytesFreed"`
	ReapedCounts map[string]int `json:"reapedCounts"`
	Errors       []string       `json:"errors"`
	LogPath      string         `json:"logPath"`
}

// GET /admin/cleanup?limit=&offset= — paginated cleaner runs + window summary.
func (h *CleanupHandler) ListRuns(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	offset := 0
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n >= 0 {
		offset = n
	}

	var total int
	if err := h.DB.QueryRow(r.Context(), `SELECT COUNT(*) FROM cleanup_runs`).Scan(&total); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	rows, err := h.DB.Query(r.Context(), `
		SELECT id, started_at, finished_at, dry_run, bytes_freed,
		       COALESCE(reaped_counts, '{}'),
		       COALESCE(errors, '[]'),
		       log_path
		  FROM cleanup_runs
		 ORDER BY started_at DESC
		 LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []cleanupRunDTO{}
	for rows.Next() {
		var run cleanupRunDTO
		var countsJSON, errorsJSON []byte
		var finishedAt *time.Time
		if err := rows.Scan(&run.ID, &run.StartedAt, &finishedAt, &run.DryRun,
			&run.BytesFreed, &countsJSON, &errorsJSON, &run.LogPath); err != nil {
			continue
		}
		run.FinishedAt = finishedAt
		if finishedAt != nil {
			run.DurationMS = finishedAt.Sub(run.StartedAt).Milliseconds()
		}
		_ = json.Unmarshal(countsJSON, &run.ReapedCounts)
		_ = json.Unmarshal(errorsJSON, &run.Errors)
		// Defensive: ensure never-nil so the JSON client sees [] / {} instead
		// of null. Spares the dashboard from .length-on-null crashes.
		if run.ReapedCounts == nil {
			run.ReapedCounts = map[string]int{}
		}
		if run.Errors == nil {
			run.Errors = []string{}
		}
		out = append(out, run)
	}

	// Roll-up over the recent window so the admin sees "last 24h freed X".
	var totalBytes int64
	totalCounts := map[string]int{}
	for _, r := range out {
		totalBytes += r.BytesFreed
		for k, v := range r.ReapedCounts {
			totalCounts[k] += v
		}
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"runs":   out,
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"window": map[string]any{
			"runs":         len(out),
			"bytesFreed":   totalBytes,
			"reapedCounts": totalCounts,
		},
	})
}

// GET /admin/cleanup/runs/{id}/log — tail the NDJSON audit log for one run.
// Filters by run_id since multiple runs may share a day-bucketed file.
//
// Query: ?tail=200 (default 200, max 5000).
func (h *CleanupHandler) RunLog(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "bad run id")
		return
	}
	tailN := 200
	if n, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && n > 0 && n <= 5000 {
		tailN = n
	}

	var logPath string
	if err := h.DB.QueryRow(r.Context(),
		`SELECT log_path FROM cleanup_runs WHERE id = $1`, runID,
	).Scan(&logPath); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "NO_RUN", "run not found")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Resolve to absolute path within StorageRoot to prevent path traversal.
	cleaned := filepath.Clean(logPath)
	absRoot, _ := filepath.Abs(h.StorageRoot)
	absLog, _ := filepath.Abs(cleaned)
	if !filePathInside(absLog, absRoot) {
		httpx.WriteError(w, http.StatusForbidden, "BAD_PATH", "log path outside storage root")
		return
	}

	f, err := os.Open(cleaned)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "NO_LOG", "log file missing")
		return
	}
	defer f.Close()

	// Stream-filter by runId. We don't have a fancy reverse-tail; for typical
	// run sizes (<10k events) this is fine. Could ring-buffer later.
	events := []map[string]any{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20) // up to 1 MB per line
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if rid, _ := m["runId"].(string); rid == runID.String() {
			events = append(events, m)
		}
	}

	// Tail
	if len(events) > tailN {
		events = events[len(events)-tailN:]
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"runId":  runID,
		"path":   cleaned,
		"count":  len(events),
		"events": events,
	})
}

// POST /admin/cleanup/run — notify the cleaner to wake up immediately.
// Uses Postgres NOTIFY because we already have a DB connection on hand; the
// cleaner listens on the same channel between ticks (added in cleaner.go).
func (h *CleanupHandler) RunNow(w http.ResponseWriter, r *http.Request) {
	if _, err := h.DB.Exec(r.Context(), `NOTIFY ps3_cleaner_wakeup`); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"message": "wake-up notification sent — next tick will start within a few seconds",
	})
}

func filePathInside(child, parent string) bool {
	if parent == "" || child == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !startsWith(rel, ".."+string(filepath.Separator))
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
