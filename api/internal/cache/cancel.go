package cache

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// TranscodeCancelChannel is the Valkey pub/sub channel that workers listen
// to for "stop your ffmpeg now" signals.
//
// Messages are JSON of one of two shapes:
//
//	{"jobId":   "<uuid>"}    — cancel one specific job
//	{"objectId":"<uuid>"}    — cancel every job for that object
//
// The worker matches against its in-flight job and SIGTERMs the local
// ffmpeg subprocess. Other workers ignore the message.
const TranscodeCancelChannel = "transcode:cancel"

// CancelMessage is one entry on the channel. At most one of JobID/ObjectID
// should be set per message.
type CancelMessage struct {
	JobID    uuid.UUID `json:"jobId,omitempty"`
	ObjectID uuid.UUID `json:"objectId,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// PublishCancelJob fires-and-forgets a cancel for one job.
// Errors are logged but never returned — cancellation is best-effort, the
// worker also notices via the row-gone check.
func PublishCancelJob(ctx context.Context, rdb *redis.Client, jobID uuid.UUID, reason string) {
	publish(ctx, rdb, CancelMessage{JobID: jobID, Reason: reason})
}

// PublishCancelObject cancels every transcode job tied to a given object.
// Used when the API deletes/replaces an object — saves enumerating job IDs.
func PublishCancelObject(ctx context.Context, rdb *redis.Client, objectID uuid.UUID, reason string) {
	publish(ctx, rdb, CancelMessage{ObjectID: objectID, Reason: reason})
}

func publish(ctx context.Context, rdb *redis.Client, msg CancelMessage) {
	if rdb == nil {
		return
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	// Use a short timeout so cancellation never blocks the request path.
	pctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := rdb.Publish(pctx, TranscodeCancelChannel, b).Err(); err != nil {
		log.Printf("cancel publish failed: %v", err)
	}
}
