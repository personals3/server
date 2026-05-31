// Package importer runs URL imports as background jobs.
//
// Architecture: same as the Python transcoder — N goroutines poll import_jobs
// with FOR UPDATE SKIP LOCKED, claim a job atomically, run the download with
// periodic DB updates for progress, and finish (or fail).
//
// Because this lives inside the API process, we get easy access to the
// existing storage.FS, quota helpers, and transcode-enqueue logic. No extra
// container needed.
package importer

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/sharding"
	"github.com/personals3/api/internal/storage"
)

type Importer struct {
	DB          *pgxpool.Pool
	FS          *storage.FS
	Concurrency int
	WorkerName  string

	// EnqueueTranscode is plugged in from cmd/server/main.go to avoid
	// importing the handlers package (would create a cycle).
	EnqueueTranscode func(ctx context.Context, objectID, bucketID uuid.UUID, key, contentType string)

	// QuotaReserve is the same function used by HTTP handlers — wired in to
	// avoid cycles.
	QuotaReserve     func(ctx context.Context, userID uuid.UUID, delta int64) error
	CheckDiskHealthy func(ctx context.Context) error
}

// Start launches N goroutines that poll for pending jobs. Returns when ctx is cancelled.
func (im *Importer) Start(ctx context.Context) {
	if im.Concurrency < 1 {
		im.Concurrency = 2
	}
	for n := 0; n < im.Concurrency; n++ {
		go im.loop(ctx, fmt.Sprintf("%s-%d", im.WorkerName, n))
	}
}

type job struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	BucketID   uuid.UUID
	Key        string
	SourceURL  string
	AuthHeader string
}

func (im *Importer) loop(ctx context.Context, workerID string) {
	log.Printf("importer %s started", workerID)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		j, err := im.claim(ctx, workerID)
		if err != nil {
			log.Printf("importer %s claim error: %v", workerID, err)
			time.Sleep(5 * time.Second)
			continue
		}
		if j == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		im.run(ctx, workerID, j)
	}
}

func (im *Importer) claim(ctx context.Context, workerID string) (*job, error) {
	var j job
	err := im.DB.QueryRow(ctx, `
		WITH next AS (
			SELECT id FROM import_jobs
			 WHERE status = 'pending'
			 ORDER BY created_at
			 LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		UPDATE import_jobs SET
			status     = 'running',
			started_at = now(),
			worker_id  = $1
		WHERE id = (SELECT id FROM next)
		RETURNING id, user_id, bucket_id, key, source_url, COALESCE(auth_header, '')`,
		workerID,
	).Scan(&j.ID, &j.UserID, &j.BucketID, &j.Key, &j.SourceURL, &j.AuthHeader)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (im *Importer) run(ctx context.Context, workerID string, j *job) {
	log.Printf("importer %s: job %s → %s/%s", workerID, j.ID, j.BucketID, j.Key)

	// Disk-full guard
	if im.CheckDiskHealthy != nil {
		if err := im.CheckDiskHealthy(ctx); err != nil {
			im.fail(ctx, j.ID, fmt.Sprintf("disk full: %v", err))
			return
		}
	}

	// Build request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.SourceURL, nil)
	if err != nil {
		im.fail(ctx, j.ID, err.Error())
		return
	}
	if j.AuthHeader != "" {
		req.Header.Set("Authorization", j.AuthHeader)
	}
	req.Header.Set("User-Agent", "PersonalS3-Import/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		im.fail(ctx, j.ID, "fetch failed: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		im.fail(ctx, j.ID, fmt.Sprintf("source returned HTTP %d", resp.StatusCode))
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	totalBytes := resp.ContentLength // -1 if unknown

	// Record total size so UI can compute percentage
	_, _ = im.DB.Exec(ctx,
		`UPDATE import_jobs SET total_bytes = NULLIF($1, -1) WHERE id = $2`,
		totalBytes, j.ID)

	// Existing object's size (so we know the net delta for quota accounting)
	var existingSize int64
	_ = im.DB.QueryRow(ctx,
		`SELECT size_bytes FROM objects WHERE bucket_id = $1 AND key = $2`,
		j.BucketID, j.Key).Scan(&existingSize)

	// Pre-reserve quota using Content-Length when known
	reserved := int64(0)
	if totalBytes >= 0 {
		reserved = totalBytes - existingSize
		if reserved > 0 && im.QuotaReserve != nil {
			if err := im.QuotaReserve(ctx, j.UserID, reserved); err != nil {
				im.fail(ctx, j.ID, "quota: "+err.Error())
				return
			}
		}
	}

	// Wrap response body in a counting reader; spawn a ticker that updates
	// progress in the DB every second. Also a cancellation poller that bails
	// if the user marked the job 'cancelled'.
	pr := &progressReader{r: resp.Body}

	progressCtx, stopProgress := context.WithCancel(ctx)
	defer stopProgress()

	go im.progressTicker(progressCtx, j.ID, pr, stopProgress)

	size, etag, err := im.FS.WriteObject(j.BucketID.String(), j.Key, pr)
	stopProgress() // ensure ticker stops promptly

	if err != nil {
		// Refund the quota reservation
		if reserved > 0 && im.QuotaReserve != nil {
			_ = im.QuotaReserve(ctx, j.UserID, -reserved)
		}
		// Distinguish user-cancelled vs hard error
		if pr.cancelled.Load() {
			im.markCancelled(ctx, j.ID)
		} else {
			im.fail(ctx, j.ID, "write: "+err.Error())
		}
		return
	}

	// Reconcile quota if actual size diverged from Content-Length
	actualDelta := size - existingSize
	if actualDelta != reserved && im.QuotaReserve != nil {
		adjustment := actualDelta - reserved
		if err := im.QuotaReserve(ctx, j.UserID, adjustment); err != nil {
			_ = im.FS.RemoveObject(j.BucketID.String(), j.Key)
			_ = im.QuotaReserve(ctx, j.UserID, -reserved)
			im.fail(ctx, j.ID, "quota over: "+err.Error())
			return
		}
	}

	// Insert object row
	storagePath := im.FS.ObjectPath(j.BucketID.String(), j.Key)

	// Detect fresh insert (vs. overwrite) for shard-index accounting.
	var existed bool
	_ = im.DB.QueryRow(ctx,
		`SELECT TRUE FROM objects WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		j.BucketID, j.Key).Scan(&existed)

	var objectID uuid.UUID
	err = im.DB.QueryRow(ctx, `
		INSERT INTO objects (bucket_id, key, size_bytes, etag, content_type, storage_path)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (bucket_id, key) DO UPDATE SET
			size_bytes   = EXCLUDED.size_bytes,
			etag         = EXCLUDED.etag,
			content_type = EXCLUDED.content_type,
			storage_path = EXCLUDED.storage_path,
			is_deleted   = false,
			updated_at   = now()
		RETURNING id`,
		j.BucketID, j.Key, size, etag, contentType, storagePath,
	).Scan(&objectID)
	if err != nil {
		_ = im.FS.RemoveObject(j.BucketID.String(), j.Key)
		_ = im.QuotaReserve(ctx, j.UserID, -actualDelta)
		im.fail(ctx, j.ID, "db: "+err.Error())
		return
	}

	if !existed {
		_ = sharding.OnObjectAdded(ctx, im.DB, j.BucketID, objectID, j.Key)
	}

	if im.EnqueueTranscode != nil {
		im.EnqueueTranscode(ctx, objectID, j.BucketID, j.Key, contentType)
	}

	_, _ = im.DB.Exec(ctx, `
		UPDATE import_jobs SET
			status      = 'done',
			done_at     = now(),
			bytes_done  = $2,
			total_bytes = $2,
			object_id   = $3
		WHERE id = $1`,
		j.ID, size, objectID)

	log.Printf("importer %s: job %s done (%d bytes)", workerID, j.ID, size)
}

// progressTicker periodically writes bytes_done + throughput to the DB and
// checks if the job got cancelled. When cancelled, it sets pr.cancelled
// which causes the next pr.Read to return an error.
func (im *Importer) progressTicker(ctx context.Context, jobID uuid.UUID, pr *progressReader, cancel context.CancelFunc) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	var lastBytes int64
	lastTime := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			cur := pr.read.Load()
			deltaB := cur - lastBytes
			deltaT := t.Sub(lastTime).Seconds()
			bps := int64(0)
			if deltaT > 0 {
				bps = int64(float64(deltaB) / deltaT)
			}
			lastBytes = cur
			lastTime = t

			// Single UPDATE that also tells us if the row was cancelled
			var status string
			err := im.DB.QueryRow(ctx, `
				UPDATE import_jobs
				   SET bytes_done = $2, throughput_bps = $3
				 WHERE id = $1
				 RETURNING status`,
				jobID, cur, bps,
			).Scan(&status)
			if err != nil {
				// Row gone? Job was deleted. Cancel.
				pr.cancelled.Store(true)
				cancel()
				return
			}
			if status == "cancelled" {
				pr.cancelled.Store(true)
				cancel()
				return
			}
		}
	}
}

func (im *Importer) fail(ctx context.Context, jobID uuid.UUID, msg string) {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	_, _ = im.DB.Exec(ctx, `
		UPDATE import_jobs SET
			status  = 'failed',
			error   = $2,
			done_at = now()
		WHERE id = $1`, jobID, msg)
}

func (im *Importer) markCancelled(ctx context.Context, jobID uuid.UUID) {
	_, _ = im.DB.Exec(ctx, `
		UPDATE import_jobs SET
			status  = 'cancelled',
			done_at = now()
		WHERE id = $1 AND status = 'running'`, jobID)
}

// progressReader counts bytes read; allows cancellation by returning EOF early
// when .cancelled is set.
type progressReader struct {
	r         io.Reader
	read      atomic.Int64
	cancelled atomic.Bool
}

func (p *progressReader) Read(b []byte) (int, error) {
	if p.cancelled.Load() {
		return 0, os.ErrClosed
	}
	n, err := p.r.Read(b)
	p.read.Add(int64(n))
	return n, err
}
