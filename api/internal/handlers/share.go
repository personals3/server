package handlers

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/auth"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/sharding"
	"github.com/personals3/api/internal/storage"
)

type ShareHandler struct {
	DB        *pgxpool.Pool
	FS        *storage.FS
	JWTSecret string
	// mpHandler is set in main.go after both handlers exist; lets the
	// signed-URL finalize endpoint delegate to the same Complete/Abort
	// code path as authenticated multipart, avoiding two implementations.
	MP *MultipartHandler
}

// ---------------- POST /:bucket/*?presign — create a share link --------

type presignReq struct {
	// How long the link should be valid, in seconds. Capped at 30 days.
	ExpiresSec int64 `json:"expiresSec"`
	// "GET" (default) or "HEAD". Future: "PUT" for upload links.
	Method string `json:"method,omitempty"`
	// If true, response sets Content-Disposition: attachment
	// (browsers always download instead of inline-rendering).
	Download bool `json:"download,omitempty"`
}

type presignResp struct {
	URL       string    `json:"url"`         // relative URL: /share/...
	ExpiresAt int64     `json:"expiresAt"`   // unix seconds
	Method    string    `json:"method"`
	ShareID   uuid.UUID `json:"shareId"`     // for revocation / management
}

// CreatePresignedURL — authenticated caller exchanges their auth for a
// shareable link to one object they own.
func (h *ShareHandler) CreatePresignedURL(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)
	if key == "" {
		httpx.WriteError(w, http.StatusBadRequest, "INVALID_KEY", "object key required")
		return
	}

	var req presignReq
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
			return
		}
	}
	if req.ExpiresSec <= 0 {
		req.ExpiresSec = 3600  // default 1 hour
	}
	if req.ExpiresSec > 60*60*24*30 {
		req.ExpiresSec = 60 * 60 * 24 * 30  // cap at 30 days
	}
	if req.Method == "" {
		req.Method = "GET"
	}
	if req.Method != "GET" && req.Method != "HEAD" && req.Method != "PUT" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_METHOD",
			"method must be 'GET', 'HEAD', or 'PUT'")
		return
	}

	// Verify the user owns the bucket.
	var bucketID uuid.UUID
	err := h.DB.QueryRow(r.Context(),
		`SELECT id FROM buckets WHERE owner_id = $1 AND name = $2`,
		u.ID, bucketName,
	).Scan(&bucketID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	// For GET/HEAD: object must already exist. For PUT: it's a future upload,
	// so we only require the bucket.
	if req.Method != "PUT" {
		var exists bool
		err = h.DB.QueryRow(r.Context(),
			`SELECT TRUE FROM objects WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
			bucketID, key).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_KEY", "object not found")
			return
		}
	}

	expiresAt := time.Now().Unix() + req.ExpiresSec
	sig := auth.SignPresignedURL(h.JWTSecret, req.Method, bucketName, key, expiresAt)

	// Persist the share so it's listable + revocable. sig_hash is the lookup
	// key — sha256(sig). We don't store the raw sig because the URL is the
	// credential; a DB compromise should not yield reusable share URLs.
	sigHash := sha256.Sum256([]byte(sig))
	var shareID uuid.UUID
	if err := h.DB.QueryRow(r.Context(), `
		INSERT INTO share_links
		  (owner_id, bucket_name, object_key, method, sig_hash, expires_at, force_download)
		VALUES ($1, $2, $3, $4, $5, to_timestamp($6), $7)
		RETURNING id`,
		u.ID, bucketName, key, req.Method, sigHash[:], expiresAt, req.Download,
	).Scan(&shareID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// URL-encode each path segment so spaces / unicode survive.
	urlPath := "/share/" + url.PathEscape(bucketName)
	for _, seg := range strings.Split(key, "/") {
		urlPath += "/" + url.PathEscape(seg)
	}
	q := make(url.Values)
	q.Set("sig", sig)
	q.Set("expires", strconv.FormatInt(expiresAt, 10))
	if req.Method != "GET" {
		q.Set("method", req.Method)
	}
	if req.Download {
		q.Set("download", "1")
	}

	httpx.WriteJSON(w, http.StatusOK, presignResp{
		URL:       urlPath + "?" + q.Encode(),
		ExpiresAt: expiresAt,
		Method:    req.Method,
		ShareID:   shareID,
	})
}

// ---------------- POST /:bucket/*?presign-multipart ---------------------

type presignMultipartReq struct {
	SizeBytes  int64 `json:"sizeBytes"`           // total file size; required
	PartSize   int64 `json:"partSize,omitempty"`  // bytes; default 5 MiB
	ExpiresSec int64 `json:"expiresSec,omitempty"`// default 1 h, cap 24 h
}

type presignMultipartPartURL struct {
	PartNumber int    `json:"partNumber"`
	URL        string `json:"url"`
}

type presignMultipartResp struct {
	UploadID    string                    `json:"uploadId"`
	Parts       []presignMultipartPartURL `json:"parts"`
	CompleteURL string                    `json:"completeUrl"`
	AbortURL    string                    `json:"abortUrl"`
	ExpiresAt   int64                     `json:"expiresAt"`
	PartSize    int64                     `json:"partSize"`
}

// CreatePresignedMultipart server-initiates a multipart upload then returns
// pre-signed PUT URLs for every part plus a pre-signed POST URL for the
// finalize step. The client uploads parts in any order, in parallel,
// without ever holding a JWT — useful for browser → bucket flows where
// you don't want to proxy multi-GB bodies through the API.
//
// The Abort URL uses the same signature scheme as finalize (a POST endpoint
// served at /share/...) and lets clients clean up on failure too.
func (h *ShareHandler) CreatePresignedMultipart(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)
	if key == "" {
		httpx.WriteError(w, http.StatusBadRequest, "INVALID_KEY", "object key required")
		return
	}

	var req presignMultipartReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	if req.SizeBytes <= 0 {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "sizeBytes is required and must be > 0")
		return
	}
	if req.PartSize <= 0 {
		req.PartSize = 5 * 1024 * 1024
	}
	if req.PartSize < 5*1024*1024 {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST", "partSize must be >= 5 MiB")
		return
	}
	if req.ExpiresSec <= 0 {
		req.ExpiresSec = 3600
	}
	if req.ExpiresSec > 24*3600 {
		req.ExpiresSec = 24 * 3600 // cap at 24h — narrower than GET because PUT is mutative
	}

	numParts := (req.SizeBytes + req.PartSize - 1) / req.PartSize
	if numParts > 10000 {
		httpx.WriteError(w, http.StatusBadRequest, "TOO_MANY_PARTS",
			"sizeBytes/partSize exceeds the 10000 part cap; use a bigger partSize")
		return
	}

	// Resolve bucket + initiate the multipart upload (server-side, before
	// signing) so we have a real uploadId to sign against.
	var bucketID uuid.UUID
	err := h.DB.QueryRow(r.Context(),
		`SELECT id FROM buckets WHERE owner_id = $1 AND name = $2`,
		u.ID, bucketName,
	).Scan(&bucketID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	uploadID := uuid.New().String()
	if _, err := h.DB.Exec(r.Context(), `
		INSERT INTO multipart_uploads (upload_id, bucket_id, key, owner_id, content_type)
		VALUES ($1, $2, $3, $4, $5)`,
		uploadID, bucketID, key, u.ID, "application/octet-stream",
	); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	// Build the on-disk tmp dir so part PUTs don't race on mkdir.
	_ = h.FS.MkdirAllMultipart(bucketID.String(), uploadID)

	expiresAt := time.Now().Unix() + req.ExpiresSec
	urlBase := "/share/" + url.PathEscape(bucketName)
	for _, seg := range strings.Split(key, "/") {
		urlBase += "/" + url.PathEscape(seg)
	}

	parts := make([]presignMultipartPartURL, 0, numParts)
	for n := int64(1); n <= numParts; n++ {
		sig := auth.SignPresignedMultipartPart(h.JWTSecret, bucketName, key,
			uploadID, int(n), expiresAt)
		q := make(url.Values)
		q.Set("sig", sig)
		q.Set("expires", strconv.FormatInt(expiresAt, 10))
		q.Set("uploadId", uploadID)
		q.Set("partNumber", strconv.FormatInt(n, 10))
		parts = append(parts, presignMultipartPartURL{
			PartNumber: int(n),
			URL:        urlBase + "?" + q.Encode(),
		})
	}

	finSig := auth.SignPresignedMultipartFinalize(h.JWTSecret, bucketName, key,
		uploadID, expiresAt)
	qf := make(url.Values)
	qf.Set("sig", finSig)
	qf.Set("expires", strconv.FormatInt(expiresAt, 10))
	qf.Set("uploadId", uploadID)
	completeURL := urlBase + "?" + qf.Encode() + "&finalize=1"
	abortURL := urlBase + "?" + qf.Encode() + "&abort=1"

	httpx.WriteJSON(w, http.StatusOK, presignMultipartResp{
		UploadID:    uploadID,
		Parts:       parts,
		CompleteURL: completeURL,
		AbortURL:    abortURL,
		ExpiresAt:   expiresAt,
		PartSize:    req.PartSize,
	})
}

// ---------------- GET /share/{bucket}/* — public serve ------------------

// ServePresigned verifies the signature and serves the object's bytes.
// No Authorization header required — the URL IS the credential.
func (h *ShareHandler) ServePresigned(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")
	// Tail of the URL is the object key (may contain "/")
	rawKey := chi.URLParam(r, "*")
	key, err := url.PathUnescape(strings.TrimPrefix(rawKey, "/"))
	if err != nil {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	sig := q.Get("sig")
	expStr := q.Get("expires")
	method := q.Get("method")
	if method == "" {
		method = "GET"
	}
	if sig == "" || expStr == "" {
		http.Error(w, "missing signature", http.StatusForbidden)
		return
	}
	expiresUnix, err := auth.ParseExpiresQS(expStr)
	if err != nil {
		http.Error(w, "bad expires", http.StatusBadRequest)
		return
	}

	// Method must match what's allowed in the signed URL
	if r.Method != method {
		http.Error(w, "method not allowed for this link", http.StatusMethodNotAllowed)
		return
	}

	// HMAC verify — proves the URL hasn't been tampered with.
	if err := auth.VerifyPresignedURL(h.JWTSecret, method, bucketName, key, sig, expiresUnix); err != nil {
		http.Error(w, "invalid or expired link", http.StatusForbidden)
		return
	}

	// Server-side state lookup — gives us revocation + the AUTHORITATIVE
	// expires_at. Without this, every sig was a perpetual cap until its
	// original expiry. With it, the owner can revoke or extend without
	// invalidating the URL itself.
	sigHash := sha256.Sum256([]byte(sig))
	var dbExpires time.Time
	var dbRevoked bool
	var shareID uuid.UUID
	var forceDownloadDB bool
	err = h.DB.QueryRow(r.Context(), `
		SELECT id, expires_at, revoked, force_download
		  FROM share_links WHERE sig_hash = $1`,
		sigHash[:],
	).Scan(&shareID, &dbExpires, &dbRevoked, &forceDownloadDB)
	if errors.Is(err, pgx.ErrNoRows) {
		// Legacy share URL issued BEFORE migration 016 — fall back to the
		// signature's own expiry (verified above). Honors the original
		// link's lifetime; no revocation possible until reissued.
	} else if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	} else {
		if dbRevoked {
			http.Error(w, "link revoked", http.StatusForbidden)
			return
		}
		if time.Now().After(dbExpires) {
			http.Error(w, "link expired", http.StatusForbidden)
			return
		}
		// DB record's force_download wins if it was overridden after issue.
		if forceDownloadDB && q.Get("download") == "" {
			r.URL.Query().Set("download", "1")
		}
		// Best-effort usage bookkeeping — fire-and-forget.
		go func(id uuid.UUID) {
			_, _ = h.DB.Exec(r.Context(), `
				UPDATE share_links
				   SET last_used_at = now(), use_count = use_count + 1
				 WHERE id = $1`, id)
		}(shareID)
	}

	// Resolve bucket via name (no ownership check — the signature is the cap).
	var bucketID uuid.UUID
	var ownerID uuid.UUID
	err = h.DB.QueryRow(r.Context(),
		`SELECT id, owner_id FROM buckets WHERE name = $1`,
		bucketName,
	).Scan(&bucketID, &ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	var size int64
	var etag, contentType string
	var updatedAt time.Time
	err = h.DB.QueryRow(r.Context(), `
		SELECT size_bytes, etag, content_type, updated_at
		  FROM objects
		 WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&size, &etag, &contentType, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("Accept-Ranges", "bytes")
	// Share URLs are revocable, so the browser can NEVER cache the response.
	// Without this, a user who revokes a link finds the same browser tab still
	// serving the file from disk cache on refresh until they clear it. The
	// triplet covers HTTP/1.0 (Pragma), HTTP/1.1 cache directives, and the
	// proxy-side hint via Expires=0.
	w.Header().Set("Cache-Control", "no-store, private, max-age=0, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	if q.Get("download") == "1" {
		// Force browser to download instead of inline render
		filename := key
		if i := strings.LastIndexByte(key, '/'); i >= 0 {
			filename = key[i+1:]
		}
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filename))
	}

	if method == "HEAD" {
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

// ---------------- PUT /share/{bucket}/* — presigned upload ------------------

// ServePresignedPUT accepts a body upload at a presigned URL whose method=PUT
// was set at sign time. The bucket owner "pays" for the bytes (quota
// reservation is against the owner_id, derived from the bucket).
//
// Flow:
//  1. Verify signature + expiry + method == PUT
//  2. Look up bucket → owner_id
//  3. Disk-health check, quota reservation
//  4. Write the body to disk atomically (FS.WriteObject)
//  5. Insert/upsert objects row
//  6. Call sharding.OnObjectAdded so the cleaner accounts for it
//
// Notable behavior differences vs. authenticated PutObject:
//   - No X-PS3-Transcode header is honored (presigned URLs are simple)
//   - We don't fire transcode auto-enqueue (the owner can do that later
//     with a regular PUT or via ?transcode)
func (h *ShareHandler) ServePresignedPUT(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")
	rawKey := chi.URLParam(r, "*")
	key, err := url.PathUnescape(strings.TrimPrefix(rawKey, "/"))
	if err != nil || key == "" {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}

	q := r.URL.Query()
	sig := q.Get("sig")
	expStr := q.Get("expires")
	if sig == "" || expStr == "" {
		http.Error(w, "missing signature", http.StatusForbidden)
		return
	}
	expiresUnix, err := auth.ParseExpiresQS(expStr)
	if err != nil {
		http.Error(w, "bad expires", http.StatusBadRequest)
		return
	}

	// Presigned multipart part upload — different signature scheme bound to
	// (uploadId, partNumber). Dispatches to the same on-disk write path as
	// the authenticated UploadPart, minus the JWT user lookup.
	if uploadID := q.Get("uploadId"); uploadID != "" {
		partStr := q.Get("partNumber")
		partN, perr := strconv.Atoi(partStr)
		if perr != nil || partN < 1 || partN > 10000 {
			http.Error(w, "bad partNumber", http.StatusBadRequest)
			return
		}
		if err := auth.VerifyPresignedMultipartPart(h.JWTSecret, bucketName, key,
			uploadID, partN, sig, expiresUnix); err != nil {
			http.Error(w, "invalid or expired part link", http.StatusForbidden)
			return
		}
		h.servePresignedMultipartPart(w, r, bucketName, key, uploadID, partN)
		return
	}

	if err := auth.VerifyPresignedURL(h.JWTSecret, "PUT", bucketName, key, sig, expiresUnix); err != nil {
		http.Error(w, "invalid or expired link", http.StatusForbidden)
		return
	}

	// Same revocation + DB-authoritative-expiry check as GET. Skips silently
	// for legacy URLs issued before migration 016.
	{
		sigHash := sha256.Sum256([]byte(sig))
		var dbExpires time.Time
		var dbRevoked bool
		err = h.DB.QueryRow(r.Context(), `
			SELECT expires_at, revoked FROM share_links WHERE sig_hash = $1`,
			sigHash[:],
		).Scan(&dbExpires, &dbRevoked)
		if err == nil {
			if dbRevoked {
				http.Error(w, "link revoked", http.StatusForbidden)
				return
			}
			if time.Now().After(dbExpires) {
				http.Error(w, "link expired", http.StatusForbidden)
				return
			}
		}
	}

	// Resolve bucket + owner — the signature already attests to ownership
	// at sign time, but we still need the owner_id for quota accounting.
	var bucketID, ownerID uuid.UUID
	err = h.DB.QueryRow(r.Context(),
		`SELECT id, owner_id FROM buckets WHERE name = $1`,
		bucketName,
	).Scan(&bucketID, &ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// Disk health gate — same protection as authenticated PUT.
	if err := middleware.CheckDiskHealthy(r.Context(), h.DB, h.FS.Root()); err != nil {
		http.Error(w, "storage full", http.StatusInsufficientStorage)
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Existing size (for upsert quota delta)
	var existingSize int64
	var hasExisting bool
	if err := h.DB.QueryRow(r.Context(),
		`SELECT size_bytes FROM objects WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key,
	).Scan(&existingSize); err == nil {
		hasExisting = true
	}

	contentLength := r.ContentLength
	reserved := int64(0)
	if contentLength >= 0 {
		reserved = contentLength - existingSize
		if reserved > 0 {
			if err := middleware.QuotaReserve(r.Context(), h.DB, ownerID, reserved); err != nil {
				if errors.Is(err, middleware.ErrQuotaExceeded) {
					http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
					return
				}
				http.Error(w, "quota error", http.StatusInternalServerError)
				return
			}
		}
	}

	size, etag, err := h.FS.WriteObject(bucketID.String(), key, r.Body)
	if err != nil {
		if reserved > 0 {
			_ = middleware.QuotaReserve(r.Context(), h.DB, ownerID, -reserved)
		}
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}

	// Reconcile if body size differed from Content-Length
	actualDelta := size - existingSize
	if actualDelta != reserved {
		adjustment := actualDelta - reserved
		if err := middleware.QuotaReserve(r.Context(), h.DB, ownerID, adjustment); err != nil {
			if errors.Is(err, middleware.ErrQuotaExceeded) {
				_ = h.FS.RemoveCurrentOnly(bucketID.String(), key)
				_ = middleware.QuotaReserve(r.Context(), h.DB, ownerID, -reserved)
				http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
				return
			}
		}
	}

	storagePath := h.FS.ObjectPath(bucketID.String(), key)
	var objectID uuid.UUID
	if err := h.DB.QueryRow(r.Context(), `
		INSERT INTO objects (bucket_id, key, size_bytes, etag, content_type, storage_path)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (bucket_id, key) DO UPDATE SET
		  size_bytes   = EXCLUDED.size_bytes,
		  etag         = EXCLUDED.etag,
		  content_type = EXCLUDED.content_type,
		  storage_path = EXCLUDED.storage_path,
		  is_deleted   = false,
		  updated_at   = now()
		RETURNING id`,
		bucketID, key, size, etag, contentType, storagePath,
	).Scan(&objectID); err != nil {
		_ = h.FS.RemoveCurrentOnly(bucketID.String(), key)
		_ = middleware.QuotaReserve(r.Context(), h.DB, ownerID, -actualDelta)
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}

	if !hasExisting {
		_ = sharding.OnObjectAdded(r.Context(), h.DB, bucketID, objectID, key)
	}

	w.Header().Set("ETag", `"`+etag+`"`)
	w.Header().Set("X-Object-Size", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusCreated)
}

// servePresignedMultipartPart writes one part for a presigned multipart
// upload. Same on-disk layout as authenticated UploadPart, same quota
// pre-reserve + defer-refund pattern, same idempotency on re-upload of
// the same partNumber. No JWT — the signature is the credential.
func (h *ShareHandler) servePresignedMultipartPart(w http.ResponseWriter, r *http.Request,
	bucketName, key, uploadID string, partNumber int,
) {
	// Resolve owner via the multipart_uploads row — the row was created by
	// the presign-multipart initiator, so it knows the legit owner.
	var bucketID, ownerID uuid.UUID
	var status string
	err := h.DB.QueryRow(r.Context(), `
		SELECT bucket_id, owner_id, status
		  FROM multipart_uploads
		 WHERE upload_id = $1 AND key = $2 AND expires_at > now()`,
		uploadID, key,
	).Scan(&bucketID, &ownerID, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "upload not found or expired", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if status != "in-progress" {
		http.Error(w, "upload no longer accepting parts", http.StatusConflict)
		return
	}
	// Sanity check: bucketName in URL matches what we initiated against.
	var realBucketName string
	_ = h.DB.QueryRow(r.Context(), `SELECT name FROM buckets WHERE id=$1`,
		bucketID).Scan(&realBucketName)
	if realBucketName != bucketName {
		http.Error(w, "bucket mismatch", http.StatusBadRequest)
		return
	}

	if err := middleware.CheckDiskHealthy(r.Context(), h.DB, h.FS.Root()); err != nil {
		http.Error(w, "storage full", http.StatusInsufficientStorage)
		return
	}

	// Pre-reserve quota using Content-Length when available, subtracting any
	// existing same-numbered part (idempotent re-upload).
	contentLength := r.ContentLength
	reserved := int64(0)
	if contentLength > 0 {
		var oldSize int64
		_ = h.DB.QueryRow(r.Context(),
			`SELECT size_bytes FROM multipart_parts WHERE upload_id=$1 AND part_number=$2`,
			uploadID, partNumber,
		).Scan(&oldSize)
		reserved = contentLength - oldSize
		if reserved > 0 {
			if err := middleware.QuotaReserve(r.Context(), h.DB, ownerID, reserved); err != nil {
				if errors.Is(err, middleware.ErrQuotaExceeded) {
					http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
					return
				}
				http.Error(w, "quota error", http.StatusInternalServerError)
				return
			}
		}
	}

	// Same defer-rollback as authenticated UploadPart — see multipart.go.
	partPath := h.FS.MultipartPartPath(bucketID.String(), uploadID, partNumber)
	var (
		success     bool
		wroteToDisk bool
	)
	defer func() {
		if success {
			return
		}
		if wroteToDisk {
			_ = os.Remove(partPath)
		}
		if reserved > 0 {
			_ = middleware.QuotaReserve(context.Background(), h.DB, ownerID, -reserved)
		}
	}()

	tmpPath := partPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		http.Error(w, "fs create", http.StatusInternalServerError)
		return
	}
	hasher := md5.New()
	mw := io.MultiWriter(f, hasher)
	n, err := io.Copy(mw, r.Body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		http.Error(w, "fs write", http.StatusInternalServerError)
		return
	}
	_ = f.Sync()
	_ = f.Close()
	if err := os.Rename(tmpPath, partPath); err != nil {
		_ = os.Remove(tmpPath)
		http.Error(w, "fs rename", http.StatusInternalServerError)
		return
	}
	wroteToDisk = true

	// Reconcile quota if actual != Content-Length.
	if reserved != 0 || contentLength <= 0 {
		var oldSize int64
		_ = h.DB.QueryRow(r.Context(),
			`SELECT size_bytes FROM multipart_parts WHERE upload_id=$1 AND part_number=$2`,
			uploadID, partNumber,
		).Scan(&oldSize)
		actualDelta := n - oldSize
		adjustment := actualDelta - reserved
		if adjustment != 0 {
			if err := middleware.QuotaReserve(r.Context(), h.DB, ownerID, adjustment); err != nil {
				if errors.Is(err, middleware.ErrQuotaExceeded) {
					http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
					return
				}
			}
			reserved += adjustment
		}
	}

	etag := hex.EncodeToString(hasher.Sum(nil))
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		http.Error(w, "tx", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(r.Context())

	var oldSize int64
	_ = tx.QueryRow(r.Context(),
		`SELECT size_bytes FROM multipart_parts WHERE upload_id=$1 AND part_number=$2 FOR UPDATE`,
		uploadID, partNumber,
	).Scan(&oldSize)
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (upload_id, part_number) DO UPDATE
		  SET etag = EXCLUDED.etag, size_bytes = EXCLUDED.size_bytes, uploaded_at = now()`,
		uploadID, partNumber, etag, n); err != nil {
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}
	if _, err := tx.Exec(r.Context(),
		`UPDATE multipart_uploads SET total_size = total_size + $1 WHERE upload_id = $2`,
		n-oldSize, uploadID); err != nil {
		http.Error(w, "db", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "commit", http.StatusInternalServerError)
		return
	}

	success = true
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
}

// ServePresignedMultipartFinalize handles POST /share/.../?finalize=1
// (complete) or POST .../?abort=1 (abort) on a presigned-multipart URL.
// Both signed with the FINALIZE variant — one credential, two verbs.
//
// Complete body: {"parts":[{partNumber:N, etag:"..."},...]}  (same as
// authenticated POST /:bucket/:key?uploadId=X).
//
// Abort body: empty.
func (h *ShareHandler) ServePresignedMultipartFinalize(w http.ResponseWriter, r *http.Request) {
	bucketName := chi.URLParam(r, "bucket")
	rawKey := chi.URLParam(r, "*")
	key, err := url.PathUnescape(strings.TrimPrefix(rawKey, "/"))
	if err != nil || key == "" {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	sig := q.Get("sig")
	expStr := q.Get("expires")
	uploadID := q.Get("uploadId")
	if sig == "" || expStr == "" || uploadID == "" {
		http.Error(w, "missing signature or uploadId", http.StatusForbidden)
		return
	}
	expiresUnix, perr := auth.ParseExpiresQS(expStr)
	if perr != nil {
		http.Error(w, "bad expires", http.StatusBadRequest)
		return
	}
	if err := auth.VerifyPresignedMultipartFinalize(h.JWTSecret, bucketName, key,
		uploadID, sig, expiresUnix); err != nil {
		http.Error(w, "invalid or expired link", http.StatusForbidden)
		return
	}

	// Resolve the real owner from the upload row. The presigned signature
	// proves authorization for this specific (bucket, key, uploadId, expiry)
	// tuple — the owner ID below is what scopes the downstream operation.
	var ownerID uuid.UUID
	if err := h.DB.QueryRow(r.Context(),
		`SELECT owner_id FROM multipart_uploads WHERE upload_id=$1 AND key=$2`,
		uploadID, key,
	).Scan(&ownerID); err != nil {
		http.Error(w, "upload not found", http.StatusNotFound)
		return
	}

	// Call the userID-explicit handler variants directly — no fake User
	// injection into the request context. The downstream code uses ownerID
	// for ownership scoping and pulls quota/role from the DB as needed.
	if _, abort := q["abort"]; abort {
		h.MP.AbortForUser(w, r, ownerID)
		return
	}
	h.MP.CompleteForUser(w, r, ownerID)
}
