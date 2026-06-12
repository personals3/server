package livefeed

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	chimw "github.com/go-chi/chi/v5/middleware"
)

// First path segments that are API namespaces, not bucket names. Anything
// else at the path root is a bucket route.
var reservedSegments = map[string]bool{
	"auth": true, "admin": true, "imports": true, "search": true,
	"shares": true, "trash": true, "healthz": true, "live": true,
}

// HTTPEvents observes every request after it completes and publishes a
// classified, SAMPLED event: upload/download/error/request. It never
// blocks or fails the request path; the only always-on work is one ring-
// buffer increment for the req/min gauge.
func (b *Broker) HTTPEvents() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			trimmed := strings.Trim(r.URL.Path, "/")
			// The stream itself and health probes are not telemetry.
			if trimmed == "live" || trimmed == "healthz" {
				next.ServeHTTP(w, r)
				return
			}

			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)

			b.countRequest()
			ts := time.Now().UnixMilli()
			segs := strings.Split(trimmed, "/")
			first := segs[0]
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}

			switch {
			case status >= 400:
				if b.allowError() {
					b.publish(ErrorEvent{Type: "error", Status: status, TS: ts})
				}
			case isUpload(r.Method, first, len(segs)):
				if b.allowTraffic() {
					b.publish(UploadEvent{Type: "upload", Size: bucketFor(r.ContentLength), TS: ts})
				}
			case r.Method == http.MethodGet && isDownloadPath(first, len(segs)):
				if b.allowTraffic() {
					b.publish(DownloadEvent{Type: "download", Size: bucketFor(int64(ww.BytesWritten())), TS: ts})
				}
			default:
				if b.allowTraffic() {
					b.publish(RequestEvent{Type: "request", TS: ts})
				}
			}
		})
	}
}

// isUpload: object PUTs — /{bucket}/{key...} (single PUT and multipart
// parts), presigned /share/{bucket}/{key...} and /s/{token} PUTs.
func isUpload(method, first string, n int) bool {
	if method != http.MethodPut || n < 2 {
		return false
	}
	if first == "share" || first == "s" {
		return true
	}
	return !reservedSegments[first] && first != "public"
}

// isDownloadPath: object GETs with a key. Bucket-level GETs (listings)
// classify as plain requests.
func isDownloadPath(first string, n int) bool {
	switch first {
	case "public", "share":
		return n >= 3 // /{ns}/{bucket}/{key...}
	case "s":
		return n >= 2 // /s/{token}
	default:
		return !reservedSegments[first] && n >= 2
	}
}

// ServeSSE handles GET /live — public, read-only, no auth. Abuse control
// is structural: global + per-IP subscriber caps, sampled publishers, and
// a stream that only ever carries the scrubbed events in events.go.
func (b *Broker) ServeSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ip := r.RemoteAddr // RealIP middleware has already rewritten this
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	ch, cancel, err := b.Subscribe(ip)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: do not buffer this response
	w.WriteHeader(http.StatusOK)
	// Client-side reconnect hint; ps3-live manages its own backoff anyway.
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()

	// Heartbeat keeps idle connections alive through Cloudflare Tunnel.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			fl.Flush()
		case <-heartbeat.C:
			fmt.Fprint(w, ": hb\n\n")
			fl.Flush()
		}
	}
}
