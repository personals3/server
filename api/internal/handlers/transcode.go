package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/cache"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/storage"
	"github.com/personals3/api/internal/sysconfig"
)

// ----------------- per-user transcode limits ------------------------------
//
// Effective limit = COALESCE(users.max_*, system_config.default_*). NULL on
// the user means "use system default", NOT "unlimited" — that's `0`. The
// resolution lives in SQL so we don't multi-trip the DB for every upload.

// transcodeLimits returns the resolved (durationSec, height) caps for the
// owner of `bucketID`. 0 in either slot means "unlimited".
func transcodeLimits(ctx context.Context, db *pgxpool.Pool, bucketID uuid.UUID) (int64, int64) {
	defaultDur := sysconfig.GetInt64(ctx, db, sysconfig.KeyDefaultMaxTranscodeDurationSeconds, 1800)
	defaultH := sysconfig.GetInt64(ctx, db, sysconfig.KeyDefaultMaxTranscodeHeight, 1080)

	var userDur, userH *int64
	_ = db.QueryRow(ctx, `
		SELECT u.max_transcode_duration_seconds, u.max_transcode_height
		  FROM users u JOIN buckets b ON b.owner_id = u.id
		 WHERE b.id = $1`, bucketID,
	).Scan(&userDur, &userH)

	dur := defaultDur
	if userDur != nil {
		dur = *userDur
	}
	h := defaultH
	if userH != nil {
		h = *userH
	}
	return dur, h
}

// ---------------- file-type detection -------------------------------------

func detectFileType(contentType, key string) string {
	ct := strings.ToLower(contentType)
	switch {
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	case strings.HasPrefix(ct, "image/"):
		return "image"
	}
	ext := strings.ToLower(filepath.Ext(key))
	if ext == "" {
		return ""
	}
	if videoExts[ext] { return "video" }
	if audioExts[ext] { return "audio" }
	if imageExts[ext] { return "image" }
	return ""
}

var (
	videoExts = map[string]bool{
		".mp4": true, ".m4v": true, ".mov": true, ".mkv": true,
		".avi": true, ".webm": true, ".flv": true, ".wmv": true,
		".mpeg": true, ".mpg": true, ".3gp": true,
	}
	audioExts = map[string]bool{
		".mp3": true, ".wav": true, ".flac": true, ".aac": true,
		".ogg": true, ".m4a": true, ".opus": true, ".wma": true,
	}
	imageExts = map[string]bool{
		".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
		".bmp": true, ".tiff": true, ".tif": true, ".webp": true,
		".heic": true, ".heif": true,
	}
)

// ---------------- decision logic ------------------------------------------

// shouldTranscodeWithMode returns true if the bucket's auto_transcode_mode
// says "yes, do this file type". Used by the importer (no http.Request).
func shouldTranscodeWithMode(mode, fileType string) bool {
	switch mode {
	case "media":
		return fileType != ""
	case "photos_only":
		return fileType == "image"
	}
	return false // "none" or unknown
}

// shouldTranscodeWithRequest is the HTTP variant — also honors the
// X-PS3-Transcode header which overrides the bucket default.
func shouldTranscodeWithRequest(r *http.Request, mode, fileType string) bool {
	switch strings.ToLower(r.Header.Get("X-PS3-Transcode")) {
	case "false", "skip", "0", "no":
		return false
	case "true", "yes", "1":
		return fileType != ""
	}
	return shouldTranscodeWithMode(mode, fileType)
}

// ---------------- enqueue (called from PutObject / multipart Complete) ----

func enqueueTranscode(
	ctx context.Context, db *pgxpool.Pool, fs *storage.FS,
	objectID, bucketID uuid.UUID, key, contentType string,
	r *http.Request,
) {
	ft := detectFileType(contentType, key)
	if ft == "" {
		return
	}
	var mode string
	_ = db.QueryRow(ctx,
		`SELECT auto_transcode_mode FROM buckets WHERE id = $1`, bucketID,
	).Scan(&mode)

	if !shouldTranscodeWithRequest(r, mode, ft) {
		return
	}
	insertTranscodeJob(ctx, db, fs, objectID, bucketID, key, ft)
}

// EnqueueTranscodeFromImporter is exposed for the importer package (avoids
// import cycle). Honors the bucket mode but not headers (no request).
func EnqueueTranscodeFromImporter(
	ctx context.Context, db *pgxpool.Pool, fs *storage.FS,
	objectID, bucketID uuid.UUID, key, contentType string,
) {
	ft := detectFileType(contentType, key)
	if ft == "" {
		return
	}
	var mode string
	_ = db.QueryRow(ctx,
		`SELECT auto_transcode_mode FROM buckets WHERE id = $1`, bucketID,
	).Scan(&mode)
	if !shouldTranscodeWithMode(mode, ft) {
		return
	}
	insertTranscodeJob(ctx, db, fs, objectID, bucketID, key, ft)
}

// VideoRung is one quality level. VBR is the H.264 video bitrate target in
// kbps; ABR is the AAC audio bitrate in kbps.
type VideoRung struct {
	Height int
	VBR    int
	ABR    int
}

// CanonicalLadder lists every standard rung we know how to encode, descending.
// The actual ladder for a given source is a SUBSET of this — see PickLadder.
//
// Bitrates assume H.264; AV1/HEVC would let us roughly halve them but the
// worker uses libx264 today.
var CanonicalLadder = []VideoRung{
	{4320, 50000, 320}, //  8K
	{2160, 18000, 256}, //  4K
	{1440, 10000, 192}, // QHD
	{1080,  5000, 192}, //  FHD
	{720,   2800, 128}, //  HD
	{480,   1200,  96}, //  SD
	{360,    600,  64}, //  low
	{240,    400,  64}, //  very low
}

// PickLadder builds the ladder for one source. Includes every canonical rung
// <= sourceHeight, ordered highest first. If the source height doesn't match
// any standard (e.g., 1200p), the source's own height is inserted at the top
// as an extra rung so the user always gets a "max quality" variant.
//
// sourceHeight=0 (probe failed / unknown) falls back to the legacy ladder
// (1080/720/480/360) and lets the worker skip any rung that would upscale.
func PickLadder(sourceHeight int) []VideoRung {
	if sourceHeight <= 0 {
		return []VideoRung{
			{1080, 5000, 192},
			{720, 2800, 128},
			{480, 1200, 96},
			{360, 600, 64},
		}
	}

	out := make([]VideoRung, 0, len(CanonicalLadder)+1)
	matched := false
	for _, r := range CanonicalLadder {
		if r.Height > sourceHeight {
			continue
		}
		if r.Height == sourceHeight {
			matched = true
		}
		out = append(out, r)
	}
	// If the source's resolution doesn't sit exactly on a canonical rung,
	// add a custom top rung at the source's height so the highest quality
	// preserves source resolution. Skip if source is below the smallest rung.
	if !matched && sourceHeight > 240 {
		topVBR, topABR := bitratesFor(sourceHeight)
		out = append([]VideoRung{{sourceHeight, topVBR, topABR}}, out...)
	}
	if len(out) == 0 {
		// Source smaller than smallest rung — at least encode something at source res.
		out = append(out, VideoRung{sourceHeight, 400, 64})
	}
	return out
}

// bitratesFor returns reasonable VBR/ABR for an arbitrary height. Linear
// interpolation against the closest canonical rung above (saturating at 8K).
func bitratesFor(height int) (int, int) {
	for _, r := range CanonicalLadder {
		if height >= r.Height {
			// Pixel-area proportional scaling against the matched rung
			ratio := float64(height*height) / float64(r.Height*r.Height)
			return int(float64(r.VBR) * ratio), r.ABR
		}
	}
	return 400, 64
}

// VideoLadder is the legacy export — kept for any external caller. New code
// should call PickLadder(sourceHeight) instead.
var VideoLadder = []VideoRung{
	{1080, 5000, 192},
	{720, 2800, 128},
	{480, 1200, 96},
	{360, 600, 64},
}

// probeVideoHeight runs `ffprobe` against the source and returns the height
// in pixels. Returns 0 on any failure — the caller MUST treat 0 as "unknown"
// and fall back to a safe ladder, never assume "no video".
//
// We use a short timeout because this runs on the upload hot path. ffprobe
// on a 100GB file completes in well under 5s for header reads.
func probeVideoHeight(path string) int {
	h, _ := probeVideoMeta(path)
	return h
}

// probeVideoMeta returns (height_pixels, duration_seconds). Either field is
// 0 if it couldn't be determined.
func probeVideoMeta(path string) (int, float64) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx,
		"ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=height:format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 {
		return 0, 0
	}
	h, _ := strconv.Atoi(strings.TrimSpace(lines[0]))
	var d float64
	if len(lines) > 1 {
		d, _ = strconv.ParseFloat(strings.TrimSpace(lines[1]), 64)
	}
	if h < 0 {
		h = 0
	}
	if d < 0 {
		d = 0
	}
	return h, d
}

// EstimateTranscodeBytes returns a conservative upper bound on the bytes
// an HLS transcode of `durationSec` produces given `ladder`. Uses the
// configured VBR + ABR per rung plus a safety factor for encoder overshoot
// and container overhead.
//
// If durationSec is 0 (probe failed), falls back to sourceBytes * fallback —
// imperfect for highly-compressed sources but bounded.
func EstimateTranscodeBytes(ladder []VideoRung, durationSec float64, sourceBytes int64) int64 {
	const safety = 1.4 // encoder overshoot + ts container overhead
	if durationSec > 0 {
		var totalKbps int
		for _, r := range ladder {
			totalKbps += r.VBR + r.ABR
		}
		bytes := float64(totalKbps) * 1000.0 / 8.0 * durationSec * safety
		return int64(bytes)
	}
	// Duration unknown — fall back to a multiple of source bytes.
	const fallback = 4.0
	return int64(float64(sourceBytes) * fallback)
}

// fetchOwnerQuotaAndUsed returns (quota_bytes, used_bytes, owner_id) for the
// owner of `bucketID`. Returns zero values on any error.
func fetchOwnerQuotaAndUsed(ctx context.Context, db *pgxpool.Pool, bucketID uuid.UUID) (int64, int64, uuid.UUID, error) {
	var quota, used int64
	var ownerID uuid.UUID
	err := db.QueryRow(ctx, `
		SELECT u.quota_bytes, u.used_bytes, u.id
		  FROM users u JOIN buckets b ON b.owner_id = u.id
		 WHERE b.id = $1`, bucketID,
	).Scan(&quota, &used, &ownerID)
	return quota, used, ownerID, err
}

// Priority defaults — lower = run first.
const (
	prioImage           = 1
	prioVideoFinalize   = 1
	prioAudio           = 2
	prioVideoThumbnails = 3
	prioVideoQuality    = 5
)

// insertTranscodeJob enqueues work for an object. Video gets fanned out into
// parallel per-quality jobs + thumbnails + a "finalize" job that waits for
// siblings. Audio/image stay as single jobs (already fast).
func insertTranscodeJob(
	ctx context.Context, db *pgxpool.Pool, fs *storage.FS,
	objectID, bucketID uuid.UUID, key, fileType string,
) {
	inputPath := fs.ObjectPath(bucketID.String(), key)
	outputDir := fs.SegmentsDir(objectID.String())

	switch fileType {
	case "video":
		if !insertVideoPipeline(ctx, db, objectID, bucketID, inputPath, outputDir) {
			return // skipped (quota); status already set to skipped_quota
		}
	case "audio":
		insertSingleJob(ctx, db, objectID, inputPath, outputDir, "audio", nil, prioAudio, "pending")
	case "image":
		insertSingleJob(ctx, db, objectID, inputPath, outputDir, "image", nil, prioImage, "pending")
	default:
		// Unknown — fall back to monolithic with original type. Worker may fail.
		insertSingleJob(ctx, db, objectID, inputPath, outputDir, fileType, nil, prioVideoQuality, "pending")
	}

	_, _ = db.Exec(ctx,
		`UPDATE objects SET transcode_status = 'pending' WHERE id = $1`, objectID)
}

// insertVideoPipeline creates N quality jobs + 1 thumbnails job + 1 finalize.
// Quality jobs run in parallel across workers. Finalize waits for all of them.
//
// The ladder is BUILT from the source's actual resolution (via ffprobe), so
// an 8K source produces 4320p/2160p/1440p/1080p/.../360p and a 480p source
// only produces 480p/360p. If the probe fails we fall back to the legacy
// 1080/720/480/360 ladder and let the worker skip upscales.
func insertVideoPipeline(
	ctx context.Context, db *pgxpool.Pool,
	objectID, bucketID uuid.UUID, inputPath, outputDir string,
) bool {
	groupID := uuid.New()

	srcHeight, durationSec := probeVideoMeta(inputPath)
	ladder := PickLadder(srcHeight)

	// ---- Per-user transcode limits ---------------------------------------
	// Duration gate: if the source is longer than the user's effective cap,
	// skip the whole pipeline. Object stays downloadable as raw bytes, just
	// no HLS. Cheap early-exit before we burn quota or enqueue jobs.
	maxDur, maxHeight := transcodeLimits(ctx, db, bucketID)
	if maxDur > 0 && durationSec > float64(maxDur) {
		detail, _ := json.Marshal(map[string]any{
			"transcodeLimit": map[string]any{
				"reason":             "skipped_duration_limit",
				"sourceDurationSec":  durationSec,
				"maxDurationSec":     maxDur,
				"sourceHeight":       srcHeight,
			},
		})
		_, _ = db.Exec(ctx, `
			UPDATE objects SET transcode_status='skipped_duration_limit',
			                   transcoded = $1::jsonb,
			                   updated_at = now()
			 WHERE id = $2`, string(detail), objectID)
		return false
	}

	// Height gate: filter the ladder to rungs ≤ user's cap. The object still
	// transcodes; it just gets fewer rungs. Worker doesn't need to know
	// about the limit — it just receives a shorter ladder.
	if maxHeight > 0 {
		filtered := ladder[:0]
		for _, r := range ladder {
			if int64(r.Height) <= maxHeight {
				filtered = append(filtered, r)
			}
		}
		ladder = filtered
		if len(ladder) == 0 {
			// Source is taller than every allowed rung AND we filtered them
			// all out. This is a corner case (e.g. maxHeight=240 on a 4K
			// source) — surface a clear status rather than an empty pipeline.
			detail, _ := json.Marshal(map[string]any{
				"transcodeLimit": map[string]any{
					"reason":         "skipped_height_limit",
					"sourceHeight":   srcHeight,
					"maxHeight":      maxHeight,
					"durationSec":    durationSec,
				},
			})
			_, _ = db.Exec(ctx, `
				UPDATE objects SET transcode_status='skipped_height_limit',
				                   transcoded = $1::jsonb,
				                   updated_at = now()
				 WHERE id = $2`, string(detail), objectID)
			return false
		}
	}

	// Pre-flight quota estimate + ATOMIC RESERVATION.
	//
	// We don't just compare used_bytes < quota — we charge the estimate
	// against used_bytes right now and stash it in
	// objects.transcode_reserved_bytes. That way two concurrent uploads
	// each see the other's reserved bytes and the second one gets
	// rejected deterministically. Closes the TOCTOU race where both
	// uploads pre-flight against the same snapshot.
	var sourceBytes int64
	_ = db.QueryRow(ctx,
		`SELECT size_bytes FROM objects WHERE id = $1`, objectID).Scan(&sourceBytes)
	estimate := EstimateTranscodeBytes(ladder, durationSec, sourceBytes)
	quota, used, ownerID, err := fetchOwnerQuotaAndUsed(ctx, db, bucketID)
	if err == nil && quota > 0 && estimate > 0 {
		reserveErr := middleware.QuotaReserve(ctx, db, ownerID, estimate)
		if reserveErr != nil {
			// Re-snapshot under the lock so we report the truth at refusal time.
			q2, u2, _ := middleware.QuotaSnapshot(ctx, db, ownerID)
			available := q2 - u2
			if available < 0 {
				available = 0
			}
			deficit := estimate - available
			if deficit < 0 {
				deficit = 0
			}
			detail, _ := json.Marshal(map[string]any{
				"quota": map[string]any{
					"estimatedBytes": estimate,
					"availableBytes": available,
					"quotaBytes":     q2,
					"usedBytes":      u2,
					"deficitBytes":   deficit,
					"durationSec":    durationSec,
					"rungs":          len(ladder),
					"reason":         "skipped_quota",
				},
			})
			_, _ = db.Exec(ctx, `
				UPDATE objects SET transcode_status='skipped_quota',
				                   transcoded = $1::jsonb,
				                   updated_at = now()
				 WHERE id = $2`, string(detail), objectID)
			return false
		}
		// Reservation succeeded — record it so the worker can settle the
		// delta at publish time, and dashboard polls already see it
		// reflected in used_bytes.
		_, _ = db.Exec(ctx, `
			UPDATE objects SET transcode_reserved_bytes = $1, updated_at = now()
			 WHERE id = $2`, estimate, objectID)
		_ = used // silence linter; kept for symmetry with old code path
	}

	for _, q := range ladder {
		params := map[string]any{
			"height": q.Height,
			"vbr":    q.VBR,
			"abr":    q.ABR,
		}
		insertSingleJob(ctx, db, objectID, inputPath, outputDir,
			fmt.Sprintf("video_quality_%dp", q.Height),
			groupAndParams(groupID, params),
			prioVideoQuality, "pending")
	}

	insertSingleJob(ctx, db, objectID, inputPath, outputDir,
		"video_thumbnails",
		groupAndParams(groupID, nil),
		prioVideoThumbnails, "pending")

	insertSingleJob(ctx, db, objectID, inputPath, outputDir,
		"video_finalize",
		groupAndParams(groupID, nil),
		prioVideoFinalize, "waiting")
	return true
}

type groupParams struct {
	GroupID uuid.UUID
	Params  map[string]any
}

func groupAndParams(g uuid.UUID, p map[string]any) *groupParams {
	return &groupParams{GroupID: g, Params: p}
}

func insertSingleJob(
	ctx context.Context, db *pgxpool.Pool,
	objectID uuid.UUID, inputPath, outputDir, fileType string,
	gp *groupParams, priority int, status string,
) {
	var groupID *uuid.UUID
	var paramsJSON []byte
	if gp != nil {
		groupID = &gp.GroupID
		if gp.Params != nil {
			paramsJSON, _ = json.Marshal(gp.Params)
		}
	}
	if paramsJSON == nil {
		paramsJSON = []byte("{}")
	}
	_, _ = db.Exec(ctx, `
		INSERT INTO transcode_jobs
		  (object_id, input_path, output_dir, file_type, group_id, params, priority, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		objectID, inputPath, outputDir, fileType, groupID, paramsJSON, priority, status)
}

// ---------------- Manual trigger / removal --------------------------------

// POST /:bucket/*?transcode — manually queue transcoding for one object,
// regardless of the bucket's auto_transcode_mode. Useful when a user uploads
// a video to a "general" bucket but later wants to stream it.
func (h *ObjectHandler) ManuallyTranscode(w http.ResponseWriter, r *http.Request) {
	_ = middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)

	bucketID, err := resolveBucket(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	var objectID uuid.UUID
	var contentType string
	err = h.DB.QueryRow(r.Context(),
		`SELECT id, content_type FROM objects WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&objectID, &contentType)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	ft := detectFileType(contentType, key)
	if ft == "" {
		httpx.WriteError(w, http.StatusBadRequest, "NOT_MEDIA",
			"this file isn't a recognized video/audio/image type")
		return
	}

	// Atomically cancel any existing pipeline for this object so we never
	// have two pipelines racing into the same segment dir, and so multiple
	// rapid clicks don't queue redundant work.
	//
	// We DELETE rather than mark cancelled because:
	//   1. The worker checks job_still_exists() and bails on missing rows
	//   2. The pipeline's "promote waiting" logic only counts active rows;
	//      removing them lets the new pipeline start cleanly
	// 'processing' rows are also deleted — the worker will see the row gone
	// on next progress check and stop ffmpeg.
	_, _ = h.DB.Exec(r.Context(),
		`DELETE FROM transcode_jobs WHERE object_id = $1`, objectID)
	// Tell any active worker to stop ffmpeg NOW so we don't race the new pipeline.
	cache.PublishCancelObject(r.Context(), h.RDB, objectID, "transcode restarted")

	// Don't wipe the segment dir here — a running ffmpeg might still be
	// writing into it. The new pipeline will overwrite individual files;
	// any stragglers from the cancelled pipeline get cleaned by the
	// background sweep (TODO) or on the next ?transcode DELETE.

	insertTranscodeJob(r.Context(), h.DB, h.FS, objectID, bucketID, key, ft)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"objectId": objectID, "key": key, "fileType": ft, "status": "pending",
	})
}

// DELETE /:bucket/*?transcode — remove all transcoded streams for one object
// (keeps the original file intact). Reclaims disk space.
func (h *ObjectHandler) DeleteTranscodes(w http.ResponseWriter, r *http.Request) {
	_ = middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)

	bucketID, err := resolveBucket(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	var objectID uuid.UUID
	err = h.DB.QueryRow(r.Context(),
		`SELECT id FROM objects WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&objectID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Cancel any in-flight workers BEFORE removing the dir / rows.
	cache.PublishCancelObject(r.Context(), h.RDB, objectID, "transcodes deleted")
	_ = os.RemoveAll(h.FS.SegmentsDir(objectID.String()))

	// Capture old transcoded_bytes AND any pre-flight reservation so both
	// are refunded. The reservation matters when this DELETE is the "Cancel
	// & restart" path — the worker would have refunded on its own when the
	// job got cancelled, but if it never picked up the job yet the
	// reservation only lives in this column.
	var transcodedBytes, reservedBytes int64
	var ownerID uuid.UUID
	_ = h.DB.QueryRow(r.Context(), `
		SELECT COALESCE(o.transcoded_bytes, 0),
		       COALESCE(o.transcode_reserved_bytes, 0),
		       b.owner_id
		  FROM objects o JOIN buckets b ON b.id = o.bucket_id
		 WHERE o.id = $1`, objectID,
	).Scan(&transcodedBytes, &reservedBytes, &ownerID)

	_, _ = h.DB.Exec(r.Context(), `
		UPDATE objects
		   SET transcoded = '{}'::jsonb,
		       transcoded_bytes = 0,
		       transcode_reserved_bytes = 0,
		       transcode_status = 'none'
		 WHERE id = $1`, objectID)

	refund := transcodedBytes + reservedBytes
	if refund > 0 {
		_ = middleware.QuotaReserve(r.Context(), h.DB, ownerID, -refund)
	}

	_, _ = h.DB.Exec(r.Context(),
		`DELETE FROM transcode_jobs WHERE object_id = $1`, objectID)

	w.WriteHeader(http.StatusNoContent)
}
