// Disk-write watcher — closes the "bot drops a file directly to disk"
// detection gap.
//
// The V4 dirty-shard sweep only walks shards whose DB updated_at has
// advanced — which only happens when the API itself writes. Files that
// appear on disk via any other path (container exec, root-mounted volume,
// compromised process) leave the shard's updated_at unchanged, so the
// sweep never visits them and the orphan sits for up to 24h (until the
// V2 bloom scan).
//
// This watcher uses Linux inotify (via fsnotify) to react to any
// filesystem CREATE/WRITE event under /storage/buckets/. On any disturbance
// it bumps the affected shard's updated_at, making it look "dirty" so the
// next cleaner tick walks it within 30 seconds.
//
// Net detection latency: filesystem event (~ms) → mark dirty (~ms) →
// next cleaner tick (~30s) → walk shard (~10ms per leaf) → reap.
// End-to-end well under a minute.
//
// Failure modes (all degrade gracefully — the V2 bloom scan still catches):
//   - inotify max_user_watches exhausted   → log warning, continue without watcher
//   - bot creates a directory we don't watch yet (race) → caught on next bloom scan
//   - non-Linux host (no inotify)          → watcher refuses to start, scheduled
//                                            sweeps still cover everything
package cleaner

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Watcher monitors STORAGE_ROOT/buckets/* for unauthorized disk writes and
// marks the affected shard dirty so the cleaner sweep picks it up.
type Watcher struct {
	db          *pgxpool.Pool
	storageRoot string
	fs          *fsnotify.Watcher
	stop        chan struct{}
	wg          sync.WaitGroup
	// debounce — coalesce a burst of events on the same shard into a single
	// UPDATE. fsnotify can fire dozens of events when a directory is copied
	// in; we don't need to hit the DB for each one.
	pending   map[string]time.Time // key = "bucketID|shardPath"
	pendingMu sync.Mutex
}

// StartWatcher boots the inotify watcher in a background goroutine. Safe to
// call even on systems without inotify — returns the watcher (which does
// nothing on error) plus the start error for logging.
//
// Pass nil for the returned *Watcher when the caller doesn't need to Stop().
func StartWatcher(ctx context.Context, db *pgxpool.Pool, storageRoot string) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create inotify watcher: %w", err)
	}

	w := &Watcher{
		db:          db,
		storageRoot: storageRoot,
		fs:          fsw,
		stop:        make(chan struct{}),
		pending:     map[string]time.Time{},
	}

	// Watch the top-level buckets/ dir to catch new bucket dirs as they appear.
	bucketsRoot := filepath.Join(storageRoot, "buckets")
	if err := os.MkdirAll(bucketsRoot, 0o755); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("mkdir %s: %w", bucketsRoot, err)
	}
	if err := fsw.Add(bucketsRoot); err != nil {
		_ = fsw.Close()
		return nil, fmt.Errorf("add watch %s: %w", bucketsRoot, err)
	}
	log.Printf("watcher: watching %s", bucketsRoot)

	// Add watches for every existing bucket's objects/ subdir.
	entries, _ := os.ReadDir(bucketsRoot)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		w.addBucketWatches(filepath.Join(bucketsRoot, e.Name()))
	}

	// Event + debounce flush goroutines.
	w.wg.Add(2)
	go w.eventLoop(ctx)
	go w.flushLoop(ctx)

	return w, nil
}

// Stop shuts down the watcher and blocks until both background goroutines exit.
func (w *Watcher) Stop() {
	close(w.stop)
	_ = w.fs.Close()
	w.wg.Wait()
}

// addBucketWatches adds inotify watches for one bucket's relevant subdirs.
// Idempotent — fsnotify.Add on an already-watched path is fine.
//
// CRITICAL: we ALSO scan existing entries in objects/ and queue them for
// dirty-marking. Closes the race where an attacker creates
// `mkdir -p buckets/{B}/objects/{H}` in one shot — fsnotify only reports
// events from the moment the watch is in place, so anything that existed
// before would otherwise be invisible to the watcher.
//
// We watch THREE levels:
//   1. The bucket dir itself  — catches stray files dropped at the root
//      (buckets/{B}/evil.sh)
//   2. objects/                — catches hash dirs being created
//   3. (versions and tmp inherit from #1 via parent-dir CREATE events)
func (w *Watcher) addBucketWatches(bucketDir string) {
	// 1. Watch the bucket dir itself. Anything dropped directly here is
	//    suspicious because the API only ever writes objects/, tmp/, and
	//    nothing else.
	if err := w.fs.Add(bucketDir); err != nil {
		log.Printf("watcher: add %s: %v", bucketDir, err)
	} else {
		log.Printf("watcher: + %s", bucketDir)
	}

	objectsDir := filepath.Join(bucketDir, "objects")
	st, err := os.Stat(objectsDir)
	if err != nil || !st.IsDir() {
		return
	}
	if err := w.fs.Add(objectsDir); err != nil {
		log.Printf("watcher: add %s: %v", objectsDir, err)
		return
	}
	log.Printf("watcher: + %s", objectsDir)

	// Derive bucket ID from the path so we can mark its shards.
	bucketID, err := uuid.Parse(filepath.Base(bucketDir))
	if err != nil {
		return
	}

	// Catch-up scan: queue every existing hash dir so the next flush bumps
	// the relevant shards. This covers both the addBucketWatches race AND
	// any orphans that existed before the cleaner started.
	entries, _ := os.ReadDir(objectsDir)
	suspicious := 0
	for _, e := range entries {
		name := e.Name()
		if len(name) == 64 && isHex(name) {
			w.queueBump(bucketID, name)
		} else {
			// Anything else (file under objects/, non-hash dir) is junk.
			suspicious++
		}
	}
	if len(entries) > 0 {
		log.Printf("watcher: %s: catch-up scan queued %d entries (%d suspicious)",
			bucketID, len(entries), suspicious)
	}
	if suspicious > 0 {
		w.bumpEverything(bucketID, fmt.Sprintf("%d non-hash entries in objects/", suspicious))
	}
}

// eventLoop drains fsnotify events. Coalesces by shard into the pending map;
// flushLoop drains the map into actual DB updates.
func (w *Watcher) eventLoop(ctx context.Context) {
	defer w.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case err, ok := <-w.fs.Errors:
			if !ok {
				return
			}
			// Common one: "no space left on device" when inotify hits
			// max_user_watches. Bump fs.inotify.max_user_watches sysctl.
			log.Printf("watcher: fsnotify error: %v", err)
		case ev, ok := <-w.fs.Events:
			if !ok {
				return
			}
			w.handleEvent(ev)
		}
	}
}

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	// Only CREATE / WRITE matter — REMOVE is the cleaner doing its job (or
	// the API), neither needs reaction.
	if ev.Op&(fsnotify.Create|fsnotify.Write) == 0 {
		return
	}

	rel, err := filepath.Rel(filepath.Join(w.storageRoot, "buckets"), ev.Name)
	if err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	parts := strings.SplitN(rel, string(filepath.Separator), 3)

	// New bucket directory appeared (e.g. via API bucket creation).
	if len(parts) == 1 {
		// Could be a regular file at the top — ignore.
		st, err := os.Stat(ev.Name)
		if err != nil || !st.IsDir() {
			return
		}
		w.addBucketWatches(ev.Name)
		return
	}

	bucketID, err := uuid.Parse(parts[0])
	if err != nil {
		return
	}

	// parts[1] should be "objects" or "tmp" (the only API-managed subdirs).
	// Anything else under buckets/{B}/ is unauthorized — mark the bucket's
	// shards all dirty so the sweep investigates.
	if parts[1] != "objects" {
		w.bumpEverything(bucketID, "unexpected subdir: "+parts[1])
		return
	}

	// CREATE event inside objects/ — parts[2] is the new hash dir name (or a
	// file directly under objects/ which is also suspicious).
	if len(parts) < 3 || parts[2] == "" {
		return
	}
	hashName := parts[2]
	// Only hex-string-of-length-64 is a legit sha256 hash dir; anything else
	// is junk and we mark the whole bucket dirty so the sweep finds it.
	if len(hashName) != 64 || !isHex(hashName) {
		w.bumpEverything(bucketID, "non-hash entry: "+hashName)
		return
	}

	w.queueBump(bucketID, hashName)
}

// queueBump schedules a dirty-bump for the shard owning this hash. Coalesces
// multiple events on the same shard into one UPDATE within the debounce window.
func (w *Watcher) queueBump(bucketID uuid.UUID, hashName string) {
	w.pendingMu.Lock()
	defer w.pendingMu.Unlock()
	key := bucketID.String() + "|" + hashName
	w.pending[key] = time.Now()
}

// bumpEverything marks every shard in a bucket as dirty. Used when the watcher
// can't reason about which shard is affected (e.g. a non-hash entry appeared).
func (w *Watcher) bumpEverything(bucketID uuid.UUID, reason string) {
	log.Printf("watcher: bucket %s %s — marking all shards dirty", bucketID, reason)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = w.db.Exec(ctx, `
		UPDATE object_shard_index
		   SET last_walk_at = NULL
		 WHERE bucket_id = $1`, bucketID)
}

// flushLoop drains the debounce map every 500ms, issuing one UPDATE per
// distinct (bucket, shard) it touches.
func (w *Watcher) flushLoop(ctx context.Context) {
	defer w.wg.Done()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.stop:
			return
		case <-tick.C:
			w.flush(ctx)
		}
	}
}

func (w *Watcher) flush(ctx context.Context) {
	w.pendingMu.Lock()
	pend := w.pending
	w.pending = map[string]time.Time{}
	w.pendingMu.Unlock()
	if len(pend) == 0 {
		return
	}

	// Group by bucket so we can run a single UPDATE per bucket.
	byBucket := map[uuid.UUID][]string{}
	for key := range pend {
		parts := strings.SplitN(key, "|", 2)
		if len(parts) != 2 {
			continue
		}
		bid, err := uuid.Parse(parts[0])
		if err != nil {
			continue
		}
		byBucket[bid] = append(byBucket[bid], parts[1])
	}

	for bid, hashes := range byBucket {
		if err := w.markShardsDirtyByHash(ctx, bid, hashes); err != nil {
			log.Printf("watcher: mark dirty failed for %s: %v", bid, err)
		}
	}
}

// markShardsDirtyByHash bumps the shards that own the given hashes for this
// bucket. Uses the same longest-prefix rule as sharding.AssignToLeaf, but
// here we know the hash (not the key) — so we match against the prefix in SQL.
//
// If the bucket has no shard rows yet (fresh bucket with zero API writes),
// the prefix-match returns nothing. We ENSURE-CREATE the root shard so the
// dirty flag has something to attach to, then bump it. Without this, brand-
// new buckets attacked before their first PUT would slip past the watcher.
func (w *Watcher) markShardsDirtyByHash(ctx context.Context, bucketID uuid.UUID, hashes []string) error {
	// Guard: if this bucket isn't in the DB anymore (orphan bucket dir that
	// the V2 reaper will wipe), don't even try to insert a shard row — the
	// FK would fail. The cleanup is happening anyway via the bucket-dir
	// orphan path.
	var exists bool
	_ = w.db.QueryRow(ctx,
		`SELECT TRUE FROM buckets WHERE id = $1`, bucketID,
	).Scan(&exists)
	if !exists {
		return nil
	}

	tx, err := w.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Ensure the bucket has at least a root shard row. Cheap (ON CONFLICT
	// DO NOTHING) and only needed once per bucket — subsequent calls no-op.
	if _, err := tx.Exec(ctx, `
		INSERT INTO object_shard_index
		  (bucket_id, shard_path, depth, is_leaf, state_hash, object_count)
		VALUES ($1, '', 0, TRUE, '\x00'::bytea, 0)
		ON CONFLICT (bucket_id, shard_path) DO NOTHING`, bucketID,
	); err != nil {
		return fmt.Errorf("ensure root shard for %s: %w", bucketID, err)
	}

	for _, hash := range hashes {
		tag, err := tx.Exec(ctx, `
			UPDATE object_shard_index
			   SET last_walk_at = NULL
			 WHERE bucket_id = $1
			   AND shard_path = (
			     SELECT shard_path FROM object_shard_index
			      WHERE bucket_id = $1
			        AND is_leaf = TRUE
			        AND $2::text LIKE shard_path || '%'
			      ORDER BY depth DESC LIMIT 1
			   )`, bucketID, hash)
		if err != nil {
			return fmt.Errorf("bump leaf for %s/%s: %w", bucketID, hash, err)
		}
		// Defensive: if no leaf matched (very narrow race after a split),
		// just bump everything for this bucket.
		if tag.RowsAffected() == 0 {
			_, _ = tx.Exec(ctx, `
				UPDATE object_shard_index
				   SET last_walk_at = NULL
				 WHERE bucket_id = $1 AND is_leaf = TRUE`, bucketID)
		}
	}
	return tx.Commit(ctx)
}

func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// IsLinuxOnly returns the canonical error when running on a host without
// inotify (Windows / Mac without fsevents binding, etc).
var ErrUnsupported = errors.New("fsnotify not supported on this OS")
