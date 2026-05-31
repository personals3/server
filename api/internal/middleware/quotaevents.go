// Live quota-events broadcaster.
//
// QuotaReserve publishes every charge/refund event to subscribed channels
// so admins can `curl -N /api/admin/quota-events` and watch races as they
// happen — exactly the missing visibility that let the multipart race
// hide for so long.
//
// In-process only; no Redis. Cheap channel-per-subscriber fan-out, drops
// slow consumers via a non-blocking send.

package middleware

import (
	"sync"
	"time"

	"github.com/google/uuid"
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
)

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

// broadcastQuotaEvent fans an event out to all current subscribers.
// Non-blocking — a full buffer drops the event for that subscriber.
func broadcastQuotaEvent(e QuotaEvent) {
	quotaSubsMu.Lock()
	defer quotaSubsMu.Unlock()
	if len(quotaSubs) == 0 {
		return
	}
	for _, ch := range quotaSubs {
		select {
		case ch <- e:
		default: // slow consumer; drop
		}
	}
}
