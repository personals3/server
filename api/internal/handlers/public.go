package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/storage"
)

// PublicHandler serves objects from buckets marked is_public=true.
//
// No Authorization header is required. The bucket flag is the sole gate.
// Routes:
//   GET  /public/{bucket}/              — list (JSON) or index.html if present
//   GET  /public/{bucket}/{key}         — raw object bytes
//   HEAD /public/{bucket}/{key}         — metadata
//
// Listing returns JSON unless the bucket has an index.html at the root, in
// which case GET /public/{bucket}/ serves that file (static site hosting).
type PublicHandler struct {
	DB *pgxpool.Pool
	FS *storage.FS
}

// resolvePublicBucket returns (id, ownerID) IF the bucket exists AND is_public.
// Returns pgx.ErrNoRows for both "doesn't exist" and "private", so we don't
// leak which is which.
func (h *PublicHandler) resolvePublicBucket(r *http.Request, name string) (uuid.UUID, uuid.UUID, error) {
	var id, ownerID uuid.UUID
	err := h.DB.QueryRow(r.Context(),
		`SELECT id, owner_id FROM buckets WHERE name = $1 AND is_public = TRUE`,
		name,
	).Scan(&id, &ownerID)
	return id, ownerID, err
}

// publicCORS — public buckets are designed to be embedded from any origin
// (static sites, image hotlinks). Permissive read CORS, no credentials.
func publicCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range, If-None-Match, If-Modified-Since")
	w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, ETag, Accept-Ranges")
}

// Serve handles GET/HEAD for /public/{bucket}/*.
// If the tail is empty, behave like a directory:
//   - serve index.html if it exists in the bucket root,
//   - otherwise return a JSON object listing.
func (h *PublicHandler) Serve(w http.ResponseWriter, r *http.Request) {
	publicCORS(w)
	bucketName := chi.URLParam(r, "bucket")
	rawKey := chi.URLParam(r, "*")
	key, err := url.PathUnescape(strings.TrimPrefix(rawKey, "/"))
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}

	bucketID, _, err := h.resolvePublicBucket(r, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// Empty key OR trailing "/" → directory request. Prefer index.html, then
	// listing. For "subdir/" requests, look for subdir/index.html.
	//
	// Clients that explicitly want the JSON listing (e.g. the /p/ explorer
	// UI) can override the index.html short-circuit by sending
	// `Accept: application/json` or `?format=json`. Static-site browsing
	// keeps working for normal requests.
	isDir := key == "" || strings.HasSuffix(key, "/")
	if isDir {
		wantJSON := r.URL.Query().Get("format") == "json" ||
			strings.Contains(r.Header.Get("Accept"), "application/json")
		if !wantJSON {
			indexKey := key + "index.html"
			if h.objectExists(r, bucketID, indexKey) {
				h.serveObject(w, r, bucketID, indexKey)
				return
			}
		}
		// JSON list (only on HEAD or GET; cheap either way)
		h.listJSON(w, r, bucketID, bucketName, key)
		return
	}

	// ?info — return JSON metadata (same shape as the authenticated
	// /:bucket/*?info endpoint, minus the per-job pipeline breakdown which is
	// only useful inside the dashboard). Used by the standalone /watch and
	// /listen player pages so they can play HLS variants without sign-in.
	if _, hasInfo := r.URL.Query()["info"]; hasInfo {
		h.objectInfo(w, r, bucketID, bucketName, key)
		return
	}

	// Plain object request
	if !h.objectExists(r, bucketID, key) {
		// Static-site nicety: try a 404.html at the bucket root, else plain 404.
		if h.objectExists(r, bucketID, "404.html") {
			w.WriteHeader(http.StatusNotFound)
			h.serveObject(w, r, bucketID, "404.html")
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	h.serveObject(w, r, bucketID, key)
}

// objectInfo returns metadata for a single object in a public bucket. Shape
// matches ObjectHandler.ObjectInfo so the dashboard /watch and /listen pages
// can use the same parsing path for both signed-in and anonymous viewers.
func (h *PublicHandler) objectInfo(w http.ResponseWriter, r *http.Request, bucketID uuid.UUID, bucketName, key string) {
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	var (
		objectID                          uuid.UUID
		size                              int64
		etag, contentType, transcodeStat  string
		updatedAt                         time.Time
		transcodedJSON                    []byte
	)
	err := h.DB.QueryRow(r.Context(), `
		SELECT id, size_bytes, etag, content_type, updated_at,
		       transcoded::text, transcode_status
		  FROM objects
		 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&objectID, &size, &etag, &contentType, &updatedAt, &transcodedJSON, &transcodeStat)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	var transcoded any
	if len(transcodedJSON) > 0 {
		_ = json.Unmarshal(transcodedJSON, &transcoded)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"objectId":        objectID,
		"bucket":          bucketName,
		"key":             key,
		"size":            size,
		"etag":            etag,
		"contentType":     contentType,
		"lastModified":    updatedAt,
		"transcoded":      transcoded,
		"transcodeStatus": transcodeStat,
		"public":          true,
	})
}

func (h *PublicHandler) objectExists(r *http.Request, bucketID uuid.UUID, key string) bool {
	var ok bool
	err := h.DB.QueryRow(r.Context(),
		`SELECT TRUE FROM objects WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key).Scan(&ok)
	return err == nil && ok
}

// serveObject streams the object bytes via http.ServeContent (handles Range,
// If-Modified-Since, ETag conditional GETs natively).
//
// Supports on-the-fly image resizing via the same ?w/?h/?fit/?q/?fmt query
// params as the authenticated endpoint, so public-explorer thumbnails work
// without an Authorization header.
func (h *PublicHandler) serveObject(w http.ResponseWriter, r *http.Request, bucketID uuid.UUID, key string) {
	var objectID uuid.UUID
	var size int64
	var etag, contentType string
	var updatedAt time.Time
	err := h.DB.QueryRow(r.Context(), `
		SELECT id, size_bytes, etag, content_type, updated_at
		  FROM objects
		 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&objectID, &size, &etag, &contentType, &updatedAt)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// If DB content_type is generic but key looks like an HTML/CSS/JS file,
	// fall back to extension-based guessing so static sites render correctly.
	if contentType == "" || contentType == "application/octet-stream" {
		if guessed := mimeByExt(key); guessed != "" {
			contentType = guessed
		}
	}

	// On-the-fly thumbnail/resize for image objects. Has to run BEFORE we
	// write any non-resize headers because serveResizedFS sets its own
	// Content-Type / Cache-Control.
	if r.Method == http.MethodGet {
		srcPath := h.FS.ObjectPath(bucketID.String(), key)
		if serveResizedFS(w, r, h.FS, objectID, srcPath, contentType) {
			return
		}
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "public, max-age=300")

	if r.URL.Query().Get("download") == "1" {
		filename := key
		if i := strings.LastIndexByte(key, '/'); i >= 0 {
			filename = key[i+1:]
		}
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filename))
	}

	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}

	file, err := h.FS.OpenObject(bucketID.String(), key)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer file.Close()
	http.ServeContent(w, r, key, updatedAt, file)
}

// listJSON — directory listing for public buckets without an index.html.
// Returns objects whose key starts with the requested prefix (which is the
// directory path including trailing slash, e.g. "photos/").
func (h *PublicHandler) listJSON(w http.ResponseWriter, r *http.Request, bucketID uuid.UUID, bucketName, prefix string) {
	rows, err := h.DB.Query(r.Context(), `
		SELECT key, size_bytes, etag, content_type, updated_at
		  FROM objects
		 WHERE bucket_id = $1
		   AND NOT is_deleted
		   AND key LIKE $2 || '%'
		 ORDER BY key
		 LIMIT 1000`, bucketID, prefix)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type item struct {
		Key          string    `json:"key"`
		Size         int64     `json:"size"`
		ETag         string    `json:"etag"`
		ContentType  string    `json:"contentType"`
		LastModified time.Time `json:"lastModified"`
	}
	out := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.Key, &it.Size, &it.ETag, &it.ContentType, &it.LastModified); err != nil {
			http.Error(w, "scan error", http.StatusInternalServerError)
			return
		}
		out = append(out, it)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"bucket":  bucketName,
		"prefix":  prefix,
		"count":   len(out),
		"objects": out,
		"public":  true,
	})
}

// Bare-minimum extension → MIME mapping for static-site hosting. We only
// override common web types when the DB row has a generic content_type;
// fall back to whatever was stored otherwise.
func mimeByExt(key string) string {
	ext := strings.ToLower(path.Ext(key))
	switch ext {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".json":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".gif":
		return "image/gif"
	case ".ico":
		return "image/x-icon"
	case ".webmanifest":
		return "application/manifest+json"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".xml":
		return "application/xml"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	}
	return ""
}
