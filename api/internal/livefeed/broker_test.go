package livefeed

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Pins the wire protocol shared with ps3-live/src/events.ts. If any of
// these fail after a change, the frontend contract is broken — update
// both sides together, and re-review the privacy surface.
func TestEventJSONShapes(t *testing.T) {
	cases := []struct {
		event any
		want  string
	}{
		{
			RequestEvent{Type: "request", TS: 1750000000000},
			`{"type":"request","ts":1750000000000}`,
		},
		{
			UploadEvent{Type: "upload", Size: SizeMedium, TS: 1750000000000},
			`{"type":"upload","size":"medium","ts":1750000000000}`,
		},
		{
			DownloadEvent{Type: "download", Size: SizeLarge, TS: 1750000000000},
			`{"type":"download","size":"large","ts":1750000000000}`,
		},
		{
			ErrorEvent{Type: "error", Status: 503, TS: 1750000000000},
			`{"type":"error","status":503,"ts":1750000000000}`,
		},
		{
			TranscodeStartEvent{Type: "transcode_start", Job: "j-a1b2c3d4e5f6", Size: SizeLarge, TS: 1750000000000},
			`{"type":"transcode_start","job":"j-a1b2c3d4e5f6","size":"large","ts":1750000000000}`,
		},
		{
			TranscodeProgressEvent{Type: "transcode_progress", Job: "j-a1b2c3d4e5f6", Pct: 47, TS: 1750000000000},
			`{"type":"transcode_progress","job":"j-a1b2c3d4e5f6","pct":47,"ts":1750000000000}`,
		},
		{
			TranscodeDoneEvent{Type: "transcode_done", Job: "j-a1b2c3d4e5f6", OK: true, TS: 1750000000000},
			`{"type":"transcode_done","job":"j-a1b2c3d4e5f6","ok":true,"ts":1750000000000}`,
		},
		{
			StatsEvent{Type: "stats", DiskUsedPct: 47.3, ReqPerMin: 128, ActiveTranscodes: 2, UptimeS: 1284732, TS: 1750000000000},
			`{"type":"stats","disk_used_pct":47.3,"req_per_min":128,"active_transcodes":2,"uptime_s":1284732,"ts":1750000000000}`,
		},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.event)
		if err != nil {
			t.Fatalf("marshal %T: %v", c.event, err)
		}
		if string(got) != c.want {
			t.Errorf("%T wire shape changed:\n got  %s\n want %s", c.event, got, c.want)
		}
		t.Logf("sample %T: %s", c.event, got)
	}
}

func TestSubscriberCaps(t *testing.T) {
	b := NewBroker()

	// Per-IP cap.
	cancels := make([]func(), 0, maxSubsPerIP)
	for i := 0; i < maxSubsPerIP; i++ {
		_, cancel, err := b.Subscribe("10.0.0.1")
		if err != nil {
			t.Fatalf("subscribe %d from same IP: %v", i, err)
		}
		cancels = append(cancels, cancel)
	}
	if _, _, err := b.Subscribe("10.0.0.1"); err != ErrTooManyFromIP {
		t.Fatalf("want ErrTooManyFromIP, got %v", err)
	}
	// Releasing one slot lets the IP back in.
	cancels[0]()
	if _, cancel, err := b.Subscribe("10.0.0.1"); err != nil {
		t.Fatalf("subscribe after cancel: %v", err)
	} else {
		cancel()
	}

	// Global cap (distinct IPs).
	for i := len(b.subs); i < maxSubscribers; i++ {
		if _, _, err := b.Subscribe(fmt.Sprintf("10.1.%d.%d", i/250, i%250)); err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
	}
	if _, _, err := b.Subscribe("10.99.99.99"); err != ErrStreamFull {
		t.Fatalf("want ErrStreamFull at capacity, got %v", err)
	}
}

func TestTrafficSamplerBudget(t *testing.T) {
	b := NewBroker()
	allowed := 0
	for i := 0; i < 1000; i++ {
		if b.allowTraffic() {
			allowed++
		}
	}
	// Burst capacity plus at most a couple of refilled tokens for the
	// microseconds this loop takes.
	if allowed < int(trafficBurst) || allowed > int(trafficBurst)+3 {
		t.Errorf("sampler allowed %d of 1000 instant events; want ~%d", allowed, int(trafficBurst))
	}
}

func TestSlowSubscriberDoesNotBlockPublish(t *testing.T) {
	b := NewBroker()
	_, cancel, err := b.Subscribe("10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	// Never read from ch; publish far past the buffer size. If publish
	// blocked on a full channel this would deadlock the test.
	for i := 0; i < subscriberBuffer*3; i++ {
		b.publish(RequestEvent{Type: "request", TS: int64(i)})
	}
}
