// Package cleaner runs periodic garbage collection: trash vacuum, abandoned
// multipart upload sweep, stuck transcode reset, stale URL imports, and
// (via the orphan subpackage) disk vs. DB reconciliation.
//
// Invariant the cleaner enforces:
//
//	Every file or directory under /storage/ must be reachable from a DB row.
//	If it isn't, it's an orphan and will be reaped.
//
// Each tick:
//  1. Acquire a Postgres advisory lock so only one cleaner runs at once
//     across the whole deployment (multiple containers / replicas safe).
//  2. Run each task in sequence; per-task failures don't abort the rest.
//  3. Write a cleanup_runs row + append per-event entries to an NDJSON log
//     under /storage/.cleanup/{date}.jsonl.
//  4. Release lock, sleep for CLEANUP_INTERVAL, repeat.
package cleaner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/storage"
	"github.com/personals3/api/internal/sysconfig"
)

// refreshLiveConfig pulls the cleaner's runtime-tunable knobs from
// system_config so the admin UI can flip them without a container
// rebuild. Static fields (Interval, retention windows) still come from
// env at startup — changing those requires a restart by design.
func (c *Cleaner) refreshLiveConfig(ctx context.Context) {
	c.cfg.DryRun = sysconfig.GetBool(ctx, c.db,
		sysconfig.KeyCleanupDryRun, c.cfg.DryRun)
	c.cfg.OrphanTwoStrike = sysconfig.GetBool(ctx, c.db,
		sysconfig.KeyOrphanTwoStrike, c.cfg.OrphanTwoStrike)
	if mins := sysconfig.GetInt64(ctx, c.db,
		sysconfig.KeyOrphanMinAgeMinutes, -1); mins >= 0 {
		c.cfg.OrphanMinAge = time.Duration(mins) * time.Minute
	}
	if hrs := sysconfig.GetInt64(ctx, c.db,
		sysconfig.KeyBloomRebuildHours, -1); hrs > 0 {
		c.cfg.BloomRebuildInterval = time.Duration(hrs) * time.Hour
	}
}

// Advisory-lock key — any 64-bit int, just needs to be unique per intent.
// Computed as a hash of "ps3-cleaner-tick" at init.
const advisoryLockKey int64 = 0x70733363_6c656e72 // "ps3cclenr" in hex-ish

// Config bundles the env-tunable knobs.
type Config struct {
	// Time between ticks. Default 1h.
	Interval time.Duration
	// If a previous tick still holds the lock, how long to wait before retry.
	RetryWait time.Duration
	// If true, log what we would reap but don't actually delete.
	DryRun bool
	// Retention windows
	TrashRetention       time.Duration
	MultipartExpiry      time.Duration
	StuckTranscodeAge    time.Duration
	StuckImportAge       time.Duration
	// Orphan scan
	OrphanScanInterval   time.Duration // how often to run the full disk walk
	OrphanMinAge         time.Duration // skip files newer than this
	BloomRebuildInterval time.Duration // how often to rebuild the membership bloom
	OrphanTwoStrike      bool
	// Safety caps
	MaxReapsPerTick int

	// Where to write the NDJSON audit logs (under StorageRoot).
	StorageRoot string
}

// LoadConfig reads env vars (with defaults).
func LoadConfig() Config {
	return Config{
		// V4: dirty-shard sweep is cheap, so we tick often. Most ticks are
		// 1 SELECT (no dirty shards) → milliseconds. Drops detection latency
		// for orphan files from hours to seconds.
		Interval:             envDuration("CLEANUP_INTERVAL", 30*time.Second),
		RetryWait:            envDuration("CLEANUP_RETRY_WAIT", 60*time.Second),
		DryRun:               envBool("CLEANUP_DRY_RUN", true), // SAFE DEFAULT — admin flips when comfortable
		TrashRetention:       envDays("TRASH_RETENTION_DAYS", 30),
		MultipartExpiry:      envDays("MULTIPART_EXPIRY_DAYS", 7),
		StuckTranscodeAge:    envDuration("STUCK_TRANSCODE_MINUTES", 120*time.Minute),
		StuckImportAge:       envDuration("IMPORT_STUCK_HOURS", 6*time.Hour),
		OrphanScanInterval:   envDuration("ORPHAN_SCAN_INTERVAL", 24*time.Hour),
		OrphanMinAge:         envDuration("ORPHAN_MIN_AGE_MINUTES", 30*time.Minute),
		BloomRebuildInterval: envDuration("BLOOM_REBUILD_INTERVAL", 6*time.Hour),
		OrphanTwoStrike:      envBool("ORPHAN_TWO_STRIKE", true),
		MaxReapsPerTick:      envInt("MAX_REAPS_PER_TICK", 10000),
		StorageRoot:          os.Getenv("STORAGE_ROOT"),
	}
}

// Cleaner owns the tick loop. One instance per cleaner container.
type Cleaner struct {
	cfg      Config
	db       *pgxpool.Pool
	fs       *storage.FS
	stop     chan struct{}
	logsDir  string
	lastFull time.Time
	// orphan two-strike state — paths seen as candidate on the previous tick.
	// Lives in memory; reset on cleaner restart. That's fine — first scan
	// after restart just rebuilds the candidate set; nothing gets deleted
	// until the second confirms.
	orphanCandidates map[string]struct{}
}

func New(cfg Config, db *pgxpool.Pool, fs *storage.FS) (*Cleaner, error) {
	logsDir := filepath.Join(cfg.StorageRoot, ".cleanup")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir audit log dir: %w", err)
	}
	return &Cleaner{
		cfg:              cfg,
		db:               db,
		fs:               fs,
		stop:             make(chan struct{}),
		logsDir:          logsDir,
		orphanCandidates: map[string]struct{}{},
	}, nil
}

// Run blocks until Stop is called (or ctx is cancelled).
// Listens for `NOTIFY ps3_cleaner_wakeup` on its DB connection so the admin
// "Run now" button can short-circuit the sleep between ticks.
func (c *Cleaner) Run(ctx context.Context) {
	log.Printf("cleaner: starting (interval=%s, dryRun=%v, root=%s)",
		c.cfg.Interval, c.cfg.DryRun, c.cfg.StorageRoot)

	// Wake-up channel — fed by either the periodic timer OR a NOTIFY.
	wakeup := make(chan struct{}, 1)
	go c.listenForNotify(ctx, wakeup)

	for {
		select {
		case <-ctx.Done():
			log.Printf("cleaner: context cancelled, exiting")
			return
		case <-c.stop:
			log.Printf("cleaner: stop signal, exiting")
			return
		default:
		}

		c.runOnce(ctx)

		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		case <-wakeup:
			log.Printf("cleaner: woken up by NOTIFY")
		case <-time.After(c.cfg.Interval):
		}
	}
}

// listenForNotify subscribes to the NOTIFY channel and pings wakeup chan.
// Reconnects on any error — the cleaner shouldn't die because the listener died.
func (c *Cleaner) listenForNotify(ctx context.Context, wakeup chan struct{}) {
	const channel = "ps3_cleaner_wakeup"
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stop:
			return
		default:
		}
		conn, err := c.db.Acquire(ctx)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
			conn.Release()
			time.Sleep(5 * time.Second)
			continue
		}
		log.Printf("cleaner: LISTEN %s", channel)
		for {
			notification, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				log.Printf("cleaner: notification wait error: %v", err)
				break
			}
			if notification != nil {
				// Non-blocking send — drop if already woken
				select {
				case wakeup <- struct{}{}:
				default:
				}
			}
		}
		conn.Release()
	}
}

func (c *Cleaner) Stop() { close(c.stop) }

// runOnce performs one tick. Acquires the advisory lock, runs every task,
// writes the audit log + cleanup_runs row.
func (c *Cleaner) runOnce(ctx context.Context) {
	// Acquire the lock — bail (after a brief wait) if another instance holds it.
	conn, err := c.db.Acquire(ctx)
	if err != nil {
		log.Printf("cleaner: acquire conn: %v", err)
		return
	}
	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1)`, advisoryLockKey,
	).Scan(&got); err != nil {
		log.Printf("cleaner: advisory_lock: %v", err)
		return
	}
	if !got {
		log.Printf("cleaner: another tick is in progress, skipping")
		time.Sleep(c.cfg.RetryWait)
		return
	}
	defer func() {
		_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	// Pull the live-tunable knobs from system_config so admin UI edits take
	// effect on the next tick (no container restart). Static defaults from
	// LoadConfig() remain the fallback.
	c.refreshLiveConfig(ctx)

	runID := uuid.New()
	logPath := filepath.Join(c.logsDir, time.Now().UTC().Format("2006-01-02")+".jsonl")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("cleaner: open audit log %s: %v", logPath, err)
		return
	}
	defer logFile.Close()

	rec := &runRecord{
		ID:        runID,
		StartedAt: time.Now().UTC(),
		LogPath:   logPath,
		DryRun:    c.cfg.DryRun,
		Counts:    map[string]int{},
		log:       logFile,
		dryRun:    c.cfg.DryRun,
	}

	if _, err := c.db.Exec(ctx, `
		INSERT INTO cleanup_runs (id, started_at, dry_run, log_path)
		VALUES ($1, $2, $3, $4)`,
		runID, rec.StartedAt, c.cfg.DryRun, logPath,
	); err != nil {
		log.Printf("cleaner: insert run row: %v", err)
		return
	}

	// ----- Time-based reapers -----
	c.safe(rec, "trash_vacuum",    func() error { return c.reapTrash(ctx, rec) })
	c.safe(rec, "multipart_sweep", func() error { return c.reapMultipart(ctx, rec) })
	c.safe(rec, "stuck_transcode", func() error { return c.reapStuckTranscodes(ctx, rec) })
	c.safe(rec, "stuck_imports",   func() error { return c.reapStuckImports(ctx, rec) })

	// ----- V4 shard sweep -----
	// Runs every tick now (cheap when nothing's dirty). The shard index
	// dirty-flag means we only walk what actually changed since last visit.
	c.safe(rec, "shard_sweep", func() error { return c.runShardSweep(ctx, rec) })

	// ----- V2 orphan disk walk — kept as a safety net behind a longer interval -----
	// V4 is the primary integrity mechanism; V2's flat-bloom scan still runs
	// occasionally to catch anything outside the shard-tracked paths
	// (segments, multipart tmp, etc).
	if time.Since(c.lastFull) >= c.cfg.OrphanScanInterval {
		c.safe(rec, "orphan_scan", func() error { return c.reapOrphans(ctx, rec) })
		c.lastFull = time.Now()
	}

	rec.FinishedAt = time.Now().UTC()
	countsJSON, _ := json.Marshal(rec.Counts)
	errsJSON, _ := json.Marshal(rec.Errors)
	if _, err := c.db.Exec(ctx, `
		UPDATE cleanup_runs SET finished_at = $1, reaped_counts = $2,
		                        bytes_freed = $3, errors = $4
		 WHERE id = $5`,
		rec.FinishedAt, countsJSON, rec.BytesFreed, errsJSON, runID,
	); err != nil {
		log.Printf("cleaner: update run row: %v", err)
	}

	log.Printf("cleaner: %s", rec.humanSummary())
}

// safe wraps a task so one failure doesn't kill the whole tick.
func (c *Cleaner) safe(rec *runRecord, name string, fn func() error) {
	defer func() {
		if r := recover(); r != nil {
			rec.Errors = append(rec.Errors, fmt.Sprintf("%s: panic: %v", name, r))
			log.Printf("cleaner: panic in %s: %v", name, r)
		}
	}()
	if err := fn(); err != nil {
		rec.Errors = append(rec.Errors, fmt.Sprintf("%s: %v", name, err))
		log.Printf("cleaner: %s failed: %v", name, err)
	}
}

// runRecord is the in-flight state for one tick. Also holds the open audit
// log file so per-event writes don't have to re-open it.
type runRecord struct {
	ID         uuid.UUID
	StartedAt  time.Time
	FinishedAt time.Time
	LogPath    string
	DryRun     bool
	Counts     map[string]int
	BytesFreed int64
	Errors     []string

	log    *os.File
	dryRun bool
}

// humanSummary renders a friendly, single-line description of what this
// tick actually did. Built from rec.Counts (machine codes) but reads like
// a sentence so operators can scan the log without decoding JSON.
//
// Quiet ticks collapse to "no work" instead of "counts={} bytes=0".
func (r *runRecord) humanSummary() string {
	dur := r.FinishedAt.Sub(r.StartedAt).Round(time.Millisecond)
	if len(r.Counts) == 0 && len(r.Errors) == 0 {
		return fmt.Sprintf("nothing to do (checked in %s)", dur)
	}
	var phrases []string
	// Order chosen to read naturally — "removed N, flagged N more,
	// scanned N shards" rather than alphabetical.
	for _, e := range []struct {
		key, singular, plural string
	}{
		// Things actually deleted on this tick.
		{"orphan_in_bucket_root",   "stray file in a bucket root",          "stray files in bucket roots"},
		{"orphan_stray_in_objects", "stray entry under objects/",           "stray entries under objects/"},
		{"orphan_multipart_tmp",    "leftover multipart upload",            "leftover multipart uploads"},
		{"orphan_upload",           "expired multipart upload",             "expired multipart uploads"},
		{"orphan_tmp_write",        "abandoned mid-write file",             "abandoned mid-write files"},
		{"orphan_hash_dir",         "orphan object directory",              "orphan object directories"},
		{"orphan_version",          "orphan version file",                  "orphan version files"},
		{"orphan_segments",         "orphan transcode segments dir",        "orphan transcode segments dirs"},
		{"orphan_bucket_dir",       "orphan bucket directory",              "orphan bucket directories"},
		{"trash_vacuum",            "purged trashed object",                "purged trashed objects"},
		{"multipart_sweep",         "aborted expired multipart upload",     "aborted expired multipart uploads"},
		{"stuck_transcode",         "reset stuck transcode job",            "reset stuck transcode jobs"},
		{"stuck_imports",           "reset stuck import job",               "reset stuck import jobs"},
		{"missing_data",            "object missing its data file",         "objects missing their data files"},
		{"shard_split",             "split shard (hot)",                    "split shards (hot)"},
		// Two-strike "first sighting"s that did NOT delete on this tick.
		{"orphan_candidate",        "leftover flagged for next tick",       "leftovers flagged for next tick"},
		{"orphan_deferred",         "deferred orphan check",                "deferred orphan checks"},
		// Pure informational summaries.
		{"shard_sweep_summary",     "shard sweep summary",                  "shard sweep summaries"},
	} {
		n := r.Counts[e.key]
		if n == 0 {
			continue
		}
		// Skip noisy info-only counters when there's other content.
		if e.key == "shard_sweep_summary" || e.key == "orphan_deferred" {
			continue
		}
		if n == 1 {
			phrases = append(phrases, "1 "+e.singular)
		} else {
			phrases = append(phrases, fmt.Sprintf("%d %s", n, e.plural))
		}
	}
	body := "nothing notable"
	if len(phrases) > 0 {
		body = strings.Join(phrases, ", ")
	}
	if r.BytesFreed > 0 {
		body += fmt.Sprintf(" — freed %s", humanBytes(r.BytesFreed))
	}
	if len(r.Errors) > 0 {
		body += fmt.Sprintf(" — %d error(s): %s", len(r.Errors), r.Errors[0])
	}
	return fmt.Sprintf("%s (in %s)", body, dur)
}

// humanBytes formats bytes the way operators read them. Not exact — 1024
// vs 1000 doesn't matter for an info-only log line.
func humanBytes(n int64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// audit appends one NDJSON entry to the log file. Cheap — single Write call.
func (r *runRecord) audit(action, targetType, targetRef, reason string, bytesFreed int64) {
	entry := map[string]any{
		"ts":          time.Now().UTC().Format(time.RFC3339Nano),
		"runId":       r.ID,
		"action":      action,
		"targetType":  targetType,
		"targetRef":   targetRef,
		"reason":      reason,
		"bytesFreed":  bytesFreed,
		"dryRun":      r.dryRun,
	}
	b, _ := json.Marshal(entry)
	b = append(b, '\n')
	_, _ = r.log.Write(b)
	r.Counts[action]++
	r.BytesFreed += bytesFreed
}

// ---------- env helpers ----------

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	// If purely numeric, treat as the duration unit hinted in the var name
	// (handles e.g. STUCK_TRANSCODE_MINUTES=120). For everything else use Go syntax.
	d, err := time.ParseDuration(v)
	if err == nil {
		return d
	}
	var n int64
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
		// best-effort: pick unit from key name suffix
		switch {
		case suffix(key, "_MINUTES"):
			return time.Duration(n) * time.Minute
		case suffix(key, "_HOURS"):
			return time.Duration(n) * time.Hour
		case suffix(key, "_DAYS"):
			return time.Duration(n) * 24 * time.Hour
		case suffix(key, "_SECONDS"):
			return time.Duration(n) * time.Second
		}
	}
	return def
}

func envDays(key string, def int) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return time.Duration(def) * 24 * time.Hour
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
		return time.Duration(n) * 24 * time.Hour
	}
	return time.Duration(def) * 24 * time.Hour
}

func suffix(s, suf string) bool {
	return len(s) >= len(suf) && s[len(s)-len(suf):] == suf
}

