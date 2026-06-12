package livefeed

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	maxSubscribers   = 64
	maxSubsPerIP     = 4
	subscriberBuffer = 32

	// Traffic events (request/upload/download) share one token bucket;
	// errors get their own small one so a request flood can't hide them.
	// Everything beyond the budget is dropped from the stream but still
	// aggregated into the req/min counter.
	trafficRate, trafficBurst = 20.0, 30.0
	errorRate, errorBurst     = 5.0, 10.0
)

var (
	ErrStreamFull    = errors.New("live stream at subscriber capacity")
	ErrTooManyFromIP = errors.New("too many live streams from this address")
)

// Broker is the in-process pub/sub hub: HTTP middleware, the stats ticker
// and the transcode poller publish into it; SSE subscribers fan out of it.
// Slow subscribers drop events rather than block publishers.
type Broker struct {
	started time.Time

	mu    sync.Mutex
	subs  map[chan []byte]string // channel → subscriber IP
	perIP map[string]int

	// Rolling 60×1s buckets for the req/min gauge. Counts EVERY request,
	// sampled or not.
	ringMu     sync.Mutex
	ringCounts [60]int
	ringStamps [60]int64

	sampMu        sync.Mutex
	trafficTokens float64
	errorTokens   float64
	lastRefill    time.Time
}

func NewBroker() *Broker {
	return &Broker{
		started:       time.Now(),
		subs:          make(map[chan []byte]string),
		perIP:         make(map[string]int),
		trafficTokens: trafficBurst,
		errorTokens:   errorBurst,
		lastRefill:    time.Now(),
	}
}

// Subscribe registers a stream consumer. The returned cancel is idempotent.
func (b *Broker) Subscribe(ip string) (<-chan []byte, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subs) >= maxSubscribers {
		return nil, nil, ErrStreamFull
	}
	if b.perIP[ip] >= maxSubsPerIP {
		return nil, nil, ErrTooManyFromIP
	}
	ch := make(chan []byte, subscriberBuffer)
	b.subs[ch] = ip
	b.perIP[ip]++

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; !ok {
			return
		}
		delete(b.subs, ch)
		if b.perIP[ip]--; b.perIP[ip] <= 0 {
			delete(b.perIP, ip)
		}
	}
	return ch, cancel, nil
}

func (b *Broker) publish(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- data:
		default: // subscriber too slow — drop, never block the hot path
		}
	}
}

// ---------------------------------------------------------------- req/min

func (b *Broker) countRequest() {
	now := time.Now().Unix()
	idx := now % 60
	b.ringMu.Lock()
	if b.ringStamps[idx] != now {
		b.ringStamps[idx] = now
		b.ringCounts[idx] = 0
	}
	b.ringCounts[idx]++
	b.ringMu.Unlock()
}

func (b *Broker) reqPerMin() int {
	now := time.Now().Unix()
	total := 0
	b.ringMu.Lock()
	for i := range b.ringCounts {
		if now-b.ringStamps[i] < 60 {
			total += b.ringCounts[i]
		}
	}
	b.ringMu.Unlock()
	return total
}

// ---------------------------------------------------------------- sampling

func (b *Broker) refillLocked() {
	now := time.Now()
	dt := now.Sub(b.lastRefill).Seconds()
	b.lastRefill = now
	b.trafficTokens = min(b.trafficTokens+trafficRate*dt, trafficBurst)
	b.errorTokens = min(b.errorTokens+errorRate*dt, errorBurst)
}

func (b *Broker) allowTraffic() bool {
	b.sampMu.Lock()
	defer b.sampMu.Unlock()
	b.refillLocked()
	if b.trafficTokens >= 1 {
		b.trafficTokens--
		return true
	}
	return false
}

func (b *Broker) allowError() bool {
	b.sampMu.Lock()
	defer b.sampMu.Unlock()
	b.refillLocked()
	if b.errorTokens >= 1 {
		b.errorTokens--
		return true
	}
	return false
}
