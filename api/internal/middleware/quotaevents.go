// Live quota-events broadcaster + persistent log.
//
// QuotaReserve publishes every charge/refund/rejected event in two ways:
//   1. Live: fanned out to in-process subscribers so admins can curl the
//      SSE endpoint and watch races as they happen.
//   2. Persistent: appended to the quota_events table so the admin Logs
//      page can paginate through historical events instead of relying on
//      whoever happened to be tailing at the time.
//
// The DB write is fired into a buffered channel and drained by a single
// background goroutine — QuotaReserve never blocks waiting for INSERT.
// If the buffer overruns (very high event rate, slow DB), the event is
// dropped from the persistent log but still reaches live subscribers.

package middleware

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// QuotaEvent is one charge or refund row pushed to subscribers.
type QuotaEvent struct {
	TS        time.Time `json:"ts"`
	UserID    uuid.UUID `json:"userId"`
	Delta     int64     `json:"delta"`     // signed, +charge / -refund
	NewBytes  int64     `json:"newBytes"`  // post-update used_bytes (0 on REJECTED)
	Caller    string    `json:"caller"`    // file:line
	Rejected  bool      `json:"rejected,omitempty"`
}

var (
	quotaSubsMu sync.Mutex
	quotaSubs   []chan QuotaEvent

	// Persistence queue — buffered so a slow DB doesn't slow QuotaReserve.
	quotaPersistCh   chan QuotaEvent
	quotaPersistOnce sync.Once
)

// StartQuotaEventPersister wires the background writer that drains
// quotaPersistCh into the quota_events table. Call once at server boot;
// subsequent calls are no-ops.
func StartQuotaEventPersister(db *pgxpool.Pool) {
	quotaPersistOnce.Do(func() {
		quotaPersistCh = make(chan QuotaEvent, 1024)
		go func() {
			for e := range quotaPersistCh {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				_, err := db.Exec(ctx, `
					INSERT INTO quota_events (ts, user_id, delta, new_bytes, caller, rejected)
					VALUES ($1, $2, $3, $4, $5, $6)`,
					e.TS, e.UserID, e.Delta, e.NewBytes, e.Caller, e.Rejected)
				cancel()
				if err != nil {
					// Don't crash on a write failure — log and keep draining.
					log.Printf("quota_events insert failed: %v", err)
				}
			}
		}()
	})
}

// SubscribeQuotaEvents returns a channel that receives every QuotaReserve
// event from this moment forward. Caller MUST call the returned cancel
// func when done — otherwise the channel sits forever and (if it goes
// unread) silently drops events without telling the producer.
//
// Buffer of 64 lets a slow consumer briefly lag without losing every
// event; if the buffer is full the broadcast does a non-blocking send
// and the event is dropped for that subscriber only.
func SubscribeQuotaEvents() (<-chan QuotaEvent, func()) {
	ch := make(chan QuotaEvent, 64)
	quotaSubsMu.Lock()
	quotaSubs = append(quotaSubs, ch)
	quotaSubsMu.Unlock()
	return ch, func() {
		quotaSubsMu.Lock()
		for i, c := range quotaSubs {
			if c == ch {
				quotaSubs = append(quotaSubs[:i], quotaSubs[i+1:]...)
				break
			}
		}
		quotaSubsMu.Unlock()
		close(ch)
	}
}

// broadcastQuotaEvent fans an event out to all current subscribers AND
// queues it for persistent storage. Both sends are non-blocking.
func broadcastQuotaEvent(e QuotaEvent) {
	// Live SSE subscribers.
	quotaSubsMu.Lock()
	for _, ch := range quotaSubs {
		select {
		case ch <- e:
		default: // slow consumer; drop
		}
	}
	quotaSubsMu.Unlock()

	// Persistent log — non-blocking. If StartQuotaEventPersister wasn't
	// called (tests, scripts) or the buffer is full, the event is lost.
	if quotaPersistCh != nil {
		select {
		case quotaPersistCh <- e:
		default:
			log.Printf("quota_events persist buffer full; dropping event for %s", e.UserID)
		}
	}
}
