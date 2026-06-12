package livefeed

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Transcode lifecycle events come from READING the jobs table on a 2s
// poll — zero changes to the worker or the queue (the brief's "prefer
// reading state over modifying the worker"). Jobs are identified on the
// wire by an opaque per-boot token, never the real id.

type jobSnapshot struct {
	status string
	pct    int
}

func (b *Broker) StartTranscodePoller(ctx context.Context, db *pgxpool.Pool) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		log.Printf("livefeed: job-token nonce: %v", err)
	}
	known := make(map[string]jobSnapshot)

	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.pollTranscodes(ctx, db, nonce, known)
			}
		}
	}()
}

func (b *Broker) pollTranscodes(
	ctx context.Context,
	db *pgxpool.Pool,
	nonce []byte,
	known map[string]jobSnapshot,
) {
	qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	// Running jobs plus a short tail of freshly-terminal ones so we catch
	// completions between polls.
	rows, err := db.Query(qctx, `
		SELECT tj.id::text, tj.status, COALESCE(tj.progress_pct, 0),
		       COALESCE(o.size_bytes, 0)
		  FROM transcode_jobs tj
		  LEFT JOIN objects o ON o.id = tj.object_id
		 WHERE tj.status = 'processing'
		    OR (tj.done_at IS NOT NULL AND tj.done_at > now() - interval '15 seconds')`)
	if err != nil {
		return // transient DB hiccup — next tick retries
	}
	defer rows.Close()

	ts := time.Now().UnixMilli()
	seen := make(map[string]bool)

	for rows.Next() {
		var id, status string
		var pct int
		var sizeBytes int64
		if err := rows.Scan(&id, &status, &pct, &sizeBytes); err != nil {
			continue
		}
		seen[id] = true
		tok := jobToken(nonce, id)
		prev, wasKnown := known[id]

		switch {
		case status == "processing" && !wasKnown:
			b.publish(TranscodeStartEvent{
				Type: "transcode_start", Job: tok, Size: bucketFor(sizeBytes), TS: ts,
			})
			known[id] = jobSnapshot{status: status, pct: pct}
		case status == "processing" && pct != prev.pct:
			b.publish(TranscodeProgressEvent{
				Type: "transcode_progress", Job: tok, Pct: pct, TS: ts,
			})
			known[id] = jobSnapshot{status: status, pct: pct}
		case status != "processing" && wasKnown && prev.status == "processing":
			// done / failed / skipped — skipped counts as success.
			b.publish(TranscodeDoneEvent{
				Type: "transcode_done", Job: tok, OK: status != "failed", TS: ts,
			})
			known[id] = jobSnapshot{status: status, pct: 100}
		}
	}

	// Jobs that vanished mid-flight (cancel-on-delete cascades the row):
	// close them out as non-events rather than leaving the furnace burning.
	for id, prev := range known {
		if seen[id] {
			continue
		}
		if prev.status == "processing" {
			b.publish(TranscodeDoneEvent{
				Type: "transcode_done", Job: jobToken(nonce, id), OK: true, TS: ts,
			})
		}
		delete(known, id)
	}
}

func jobToken(nonce []byte, id string) string {
	sum := sha256.Sum256(append(nonce, id...))
	return "j-" + hex.EncodeToString(sum[:6])
}
