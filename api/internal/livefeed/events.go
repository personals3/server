// Package livefeed broadcasts a privacy-scrubbed stream of system activity
// over SSE (GET /live) for the public visualization at
// live.personals3.tech.
//
// PRIVACY IS STRUCTURAL: the event types below are the complete wire
// surface, and they only carry a type, a coarse size bucket, opaque job
// tokens, and timestamps. No filenames, bucket names, object keys, user
// ids, or IPs exist as fields — there is nothing to leak. Keep it that
// way: any new field needs the same scrutiny.
//
// The protocol is mirrored by ps3-live/src/events.ts; broker_test.go pins
// the exact JSON shapes so a drift between the two fails the build.
package livefeed

// SizeBucket is deliberately coarse — enough to vary the visuals, far too
// coarse to identify a payload.
type SizeBucket string

const (
	SizeSmall  SizeBucket = "small"  // < 8 MiB
	SizeMedium SizeBucket = "medium" // < 128 MiB
	SizeLarge  SizeBucket = "large"
)

func bucketFor(n int64) SizeBucket {
	switch {
	case n < 0:
		return SizeSmall // unknown length — assume small
	case n < 8<<20:
		return SizeSmall
	case n < 128<<20:
		return SizeMedium
	default:
		return SizeLarge
	}
}

// Timestamps are Unix milliseconds in every event.

type RequestEvent struct {
	Type string `json:"type"` // "request"
	TS   int64  `json:"ts"`
}

type UploadEvent struct {
	Type string     `json:"type"` // "upload"
	Size SizeBucket `json:"size"`
	TS   int64      `json:"ts"`
}

type DownloadEvent struct {
	Type string     `json:"type"` // "download"
	Size SizeBucket `json:"size"`
	TS   int64      `json:"ts"`
}

type ErrorEvent struct {
	Type   string `json:"type"` // "error"
	Status int    `json:"status"`
	TS     int64  `json:"ts"`
}

type TranscodeStartEvent struct {
	Type string     `json:"type"` // "transcode_start"
	Job  string     `json:"job"`  // opaque token, NOT the job id
	Size SizeBucket `json:"size"`
	TS   int64      `json:"ts"`
}

type TranscodeProgressEvent struct {
	Type string `json:"type"` // "transcode_progress"
	Job  string `json:"job"`
	Pct  int    `json:"pct"`
	TS   int64  `json:"ts"`
}

type TranscodeDoneEvent struct {
	Type string `json:"type"` // "transcode_done"
	Job  string `json:"job"`
	OK   bool   `json:"ok"`
	TS   int64  `json:"ts"`
}

type StatsEvent struct {
	Type             string  `json:"type"` // "stats"
	DiskUsedPct      float64 `json:"disk_used_pct"`
	ReqPerMin        int     `json:"req_per_min"`
	ActiveTranscodes int     `json:"active_transcodes"`
	UptimeS          int64   `json:"uptime_s"`
	TS               int64   `json:"ts"`
}
