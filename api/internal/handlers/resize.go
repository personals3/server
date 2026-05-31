package handlers

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/personals3/api/internal/storage"
)

// On-the-fly image resize.
//
// Triggered by adding any of these query params to a normal GET on an image
// object:
//
//	?w=600           — target width in pixels
//	?h=300           — target height in pixels
//	?fit=cover       — cover (crop), contain (fit-inside), scale (default)
//	?q=85            — JPEG quality 1..100 (default 85)
//	?fmt=jpg         — output format: jpg (default), webp, png
//
// Cached on disk at: segments/{objectID}/_thumbs/{w}x{h}_{fit}_{q}.{fmt}
// First request generates via ffmpeg; subsequent requests are served from
// cache via http.ServeContent.
//
// Anti-DoS:
//   - Output capped at 4096x4096 to avoid memory blowups
//   - Quality clamped to 1..100
//   - fit values restricted; anything else silently coerced to "scale"
//   - Only image content types (image/jpeg|png|webp|avif|gif) accepted
//
// Returns true if the request was a resize request (caller should NOT also
// serve the original). Returns false to let the caller fall through to the
// normal Get path.

type resizeParams struct {
	W, H    int
	Fit     string // "cover" | "contain" | "scale"
	Quality int
	Format  string // "jpg" | "webp" | "png"
}

func parseResizeParams(q map[string][]string) (resizeParams, bool) {
	p := resizeParams{Quality: 85, Fit: "scale", Format: "jpg"}
	any := false

	if v := first(q, "w"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.W = clampInt(n, 1, 4096)
			any = true
		}
	}
	if v := first(q, "h"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			p.H = clampInt(n, 1, 4096)
			any = true
		}
	}
	if v := first(q, "fit"); v != "" {
		switch v {
		case "cover", "contain", "scale":
			p.Fit = v
		}
		any = true
	}
	if v := first(q, "q"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.Quality = clampInt(n, 1, 100)
		}
	}
	if v := first(q, "fmt"); v != "" {
		switch v {
		case "jpg", "jpeg":
			p.Format = "jpg"
		case "webp":
			p.Format = "webp"
		case "png":
			p.Format = "png"
		}
	}
	return p, any
}

func first(q map[string][]string, k string) string {
	if vs, ok := q[k]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (p resizeParams) cacheName() string {
	return fmt.Sprintf("%dx%d_%s_q%d.%s", p.W, p.H, p.Fit, p.Quality, p.Format)
}

// ffmpegVideoFilter builds the -vf expression for the resize. ffmpeg's
// scale + crop combo covers all our modes.
func (p resizeParams) ffmpegVideoFilter() string {
	switch p.Fit {
	case "cover":
		// Scale so the smaller side matches, then center-crop.
		// If only one dimension is given, treat it as a square target.
		w, h := p.W, p.H
		if w == 0 {
			w = h
		}
		if h == 0 {
			h = w
		}
		return fmt.Sprintf(
			"scale=w='if(gt(a*%d,%d),-1,%d)':h='if(gt(a*%d,%d),%d,-1)',crop=%d:%d",
			h, w, w, h, w, h, w, h,
		)
	case "contain":
		// Fit inside the box, keep aspect ratio.
		w, h := p.W, p.H
		if w == 0 {
			w = -1
		}
		if h == 0 {
			h = -1
		}
		return fmt.Sprintf("scale=w=%d:h=%d:force_original_aspect_ratio=decrease", w, h)
	default: // scale
		w, h := p.W, p.H
		if w == 0 {
			w = -1
		}
		if h == 0 {
			h = -1
		}
		return fmt.Sprintf("scale=w=%d:h=%d", w, h)
	}
}

func isImageContentType(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "image/")
}

// resizeImage runs ffmpeg to produce the cached variant if it's not already
// on disk. Caller is expected to then serve the cached path.
//
// Returns the absolute path to the cached image, or an error.
//
// Free function (not on ObjectHandler) so PublicHandler can call it too —
// public buckets get the same on-the-fly thumbnail support as authenticated
// reads.
func resizeImageFS(ctx context.Context, fs *storage.FS,
	objectID uuid.UUID, srcPath string, p resizeParams,
) (string, error) {
	cacheDir := filepath.Join(fs.SegmentsDir(objectID.String()), "_thumbs")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cache: %w", err)
	}
	cachePath := filepath.Join(cacheDir, p.cacheName())
	// Already cached?
	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, nil
	}

	// Encoder params per format
	var encArgs []string
	switch p.Format {
	case "webp":
		encArgs = []string{"-c:v", "libwebp", "-quality", strconv.Itoa(p.Quality)}
	case "png":
		encArgs = []string{"-c:v", "png"}
	default: // jpg
		// ffmpeg's -q:v for mjpeg: 2..31, lower = better. Map 1-100 to 31-2.
		ffq := strconv.Itoa(int(31.0 - (float64(p.Quality-1) * 29.0 / 99.0)))
		encArgs = []string{"-c:v", "mjpeg", "-q:v", ffq}
	}

	args := []string{
		"-y", "-loglevel", "error",
		"-i", srcPath,
		"-vf", p.ffmpegVideoFilter(),
		"-frames:v", "1",
	}
	args = append(args, encArgs...)
	args = append(args, cachePath+".tmp."+p.Format)

	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, "ffmpeg", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(cachePath + ".tmp." + p.Format)
		return "", fmt.Errorf("ffmpeg resize: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// Atomic rename into place
	if err := os.Rename(cachePath+".tmp."+p.Format, cachePath); err != nil {
		return "", fmt.Errorf("rename: %w", err)
	}
	return cachePath, nil
}

// serveResizedFS is called when a request carries resize params. Returns
// true if it handled the response; the caller then returns. Works for both
// authenticated and public object reads.
func serveResizedFS(w http.ResponseWriter, r *http.Request, fs *storage.FS,
	objectID uuid.UUID, srcPath, contentType string,
) bool {
	p, any := parseResizeParams(r.URL.Query())
	if !any {
		return false
	}
	if !isImageContentType(contentType) {
		http.Error(w, "resize is only supported on images", http.StatusBadRequest)
		return true
	}
	if p.W == 0 && p.H == 0 {
		http.Error(w, "must specify at least one of w or h", http.StatusBadRequest)
		return true
	}

	cachePath, err := resizeImageFS(r.Context(), fs, objectID, srcPath, p)
	if err != nil {
		http.Error(w, "resize failed: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	f, err := os.Open(cachePath)
	if err != nil {
		http.Error(w, "cache open failed", http.StatusInternalServerError)
		return true
	}
	defer f.Close()
	info, _ := f.Stat()

	mime := "image/jpeg"
	switch p.Format {
	case "webp":
		mime = "image/webp"
	case "png":
		mime = "image/png"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if info != nil {
		http.ServeContent(w, r, p.cacheName(), info.ModTime(), f)
	}
	return true
}
