package handlers

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/sharding"
	"github.com/personals3/api/internal/storage"
)

type MultipartHandler struct {
	DB *pgxpool.Pool
	FS *storage.FS
}

// ---------- DTOs ------------------------------------------------------------

type InitiateResp struct {
	Bucket   string `json:"bucket"`
	Key      string `json:"key"`
	UploadID string `json:"uploadId"`
}

type PartInfo struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size,omitempty"`
}

type CompleteReq struct {
	Parts []PartInfo `json:"parts"`
}

type CompleteResp struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

type ListPartsResp struct {
	UploadID string     `json:"uploadId"`
	Parts    []PartInfo `json:"parts"`
}

// ---------- Helpers ---------------------------------------------------------

// loadUpload fetches an upload owned by the current user (from context).
// Used by HTTP handlers in the authenticated chain. For the signed-URL path
// (no context user), use loadUploadByID with an explicit userID.
func loadUpload(r *http.Request, db *pgxpool.Pool, uploadID string) (bucketID uuid.UUID, key, contentType string, err error) {
	u := middleware.MustUser(r.Context())
	return loadUploadByID(r.Context(), db, uploadID, u.ID)
}

// loadUploadByID is the userID-explicit variant — callable from any context,
// including the signed-URL multipart finalize path which has no
// Authenticator-injected user.
func loadUploadByID(ctx context.Context, db *pgxpool.Pool, uploadID string, userID uuid.UUID) (bucketID uuid.UUID, key, contentType string, err error) {
	err = db.QueryRow(ctx, `
		SELECT bucket_id, key, content_type
		  FROM multipart_uploads
		 WHERE upload_id = $1 AND owner_id = $2 AND status = 'in-progress'
		   AND expires_at > now()`,
		uploadID, userID,
	).Scan(&bucketID, &key, &contentType)
	return
}

// ---------- POST /:bucket/*?uploads — initiate -----------------------------

func (h *MultipartHandler) Initiate(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")
	key := keyParam(r)
	if key == "" {
		httpx.WriteError(w, http.StatusBadRequest, "INVALID_KEY", "object key required")
		return
	}

	bucketID, err := resolveBucket(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	uploadID := uuid.New().String()

	if _, err := h.DB.Exec(r.Context(), `
		INSERT INTO multipart_uploads (upload_id, bucket_id, key, owner_id, content_type)
		VALUES ($1, $2, $3, $4, $5)`,
		uploadID, bucketID, key, u.ID, contentType); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if err := os.MkdirAll(h.FS.MultipartDir(bucketID.String(), uploadID), 0o755); err != nil {
		_, _ = h.DB.Exec(r.Context(),
			`DELETE FROM multipart_uploads WHERE upload_id = $1`, uploadID)
		httpx.WriteError(w, http.StatusInternalServerError, "FS", err.Error())
		return
	}

	if isS3Client(r) {
		writeXML(w, http.StatusOK, initiateMultipartUploadResult{
			XMLNS: s3XMLNS, Bucket: bucketName, Key: key, UploadID: uploadID,
		})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, InitiateResp{
		Bucket: bucketName, Key: key, UploadID: uploadID,
	})
}

// ---------- PUT /:bucket/*?partNumber=N&uploadId=X — upload one part ------

func (h *MultipartHandler) UploadPart(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")

	if err := middleware.CheckDiskHealthy(r.Context(), h.DB, h.FS.Root()); err != nil {
		httpx.WriteError(w, http.StatusInsufficientStorage, "DISK_FULL",
			"system storage is too full to accept new uploads")
		return
	}

	uploadID := r.URL.Query().Get("uploadId")
	partStr := r.URL.Query().Get("partNumber")
	partNumber, err := strconv.Atoi(partStr)
	if err != nil || partNumber < 1 || partNumber > 10000 {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_PART_NUMBER",
			"partNumber must be 1-10000")
		return
	}

	bucketID, _, _, err := loadUpload(r, h.DB, uploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_UPLOAD",
			"upload not found, expired, or not yours")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Resolve bucket name → must match upload's bucket
	wantedBucketID, err := resolveBucket(r, h.DB, bucketName)
	if err != nil || wantedBucketID != bucketID {
		httpx.WriteError(w, http.StatusBadRequest, "BUCKET_MISMATCH",
			"upload was initiated against a different bucket")
		return
	}

	// Pre-reserve quota using Content-Length when available.
	contentLength := r.ContentLength
	reserved := int64(0)
	if contentLength > 0 {
		// Subtract any existing part of the same number (re-upload of a part).
		var oldSize int64
		_ = h.DB.QueryRow(r.Context(),
			`SELECT size_bytes FROM multipart_parts WHERE upload_id = $1 AND part_number = $2`,
			uploadID, partNumber,
		).Scan(&oldSize)
		reserved = contentLength - oldSize
		if reserved > 0 {
			if err := middleware.QuotaReserve(r.Context(), h.DB, u.ID, reserved); err != nil {
				if errors.Is(err, middleware.ErrQuotaExceeded) {
					details := middleware.QuotaErrorDetails(r.Context(), h.DB, u.ID, reserved)
					httpx.WriteErrorDetails(w, http.StatusInsufficientStorage, "QUOTA_EXCEEDED",
						"part would exceed your storage quota", details)
					return
				}
				httpx.WriteError(w, http.StatusInternalServerError, "QUOTA", err.Error())
				return
			}
		}
	}

	// success flips to true only when we've successfully committed both the
	// disk file AND the multipart_parts/multipart_uploads rows. Any earlier
	// return — including context-cancellation racing the client's Abort —
	// triggers the rollback: refund the reservation and remove the on-disk
	// part. Use context.Background() because r.Context() may already be
	// cancelled (that's often why we're here).
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
			_ = middleware.QuotaReserve(context.Background(), h.DB, u.ID, -reserved)
		}
	}()

	// Write part to disk, streaming + MD5 in one pass.
	tmpPath := partPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "FS_CREATE", err.Error())
		return
	}
	hasher := md5.New()
	mw := io.MultiWriter(f, hasher)
	n, err := io.Copy(mw, r.Body)
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		httpx.WriteError(w, http.StatusInternalServerError, "FS_WRITE", err.Error())
		return
	}
	_ = f.Sync()
	_ = f.Close()
	if err := os.Rename(tmpPath, partPath); err != nil {
		_ = os.Remove(tmpPath)
		httpx.WriteError(w, http.StatusInternalServerError, "FS_RENAME", err.Error())
		return
	}
	wroteToDisk = true

	// Reconcile quota if actual size diverged from Content-Length.
	if reserved != 0 || contentLength <= 0 {
		actualDelta := n
		var oldSize int64
		_ = h.DB.QueryRow(r.Context(),
			`SELECT size_bytes FROM multipart_parts WHERE upload_id = $1 AND part_number = $2`,
			uploadID, partNumber,
		).Scan(&oldSize)
		actualDelta -= oldSize
		adjustment := actualDelta - reserved
		if adjustment != 0 {
			if err := middleware.QuotaReserve(r.Context(), h.DB, u.ID, adjustment); err != nil {
				if errors.Is(err, middleware.ErrQuotaExceeded) {
					details := middleware.QuotaErrorDetails(r.Context(), h.DB, u.ID, adjustment)
					httpx.WriteErrorDetails(w, http.StatusInsufficientStorage, "QUOTA_EXCEEDED",
						"part exceeded your storage quota", details)
					return
				}
			}
			// On successful adjustment, fold it into `reserved` so the defer
			// refunds the right amount if a later step fails.
			reserved += adjustment
		}
	}

	etag := hex.EncodeToString(hasher.Sum(nil))

	// Upsert the part. ON CONFLICT replaces a re-uploaded part.
	// Use context.Background() for the DB write because the client may
	// have already cancelled — we still want this transaction to land so
	// the part survives in DB. If that's not desired, we get the
	// orphan-on-disk path via the defer above (refund + remove).
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "TX", err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	var oldSize int64
	_ = tx.QueryRow(r.Context(),
		`SELECT size_bytes FROM multipart_parts WHERE upload_id = $1 AND part_number = $2 FOR UPDATE`,
		uploadID, partNumber,
	).Scan(&oldSize)

	if _, err := tx.Exec(r.Context(), `
		INSERT INTO multipart_parts (upload_id, part_number, etag, size_bytes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (upload_id, part_number) DO UPDATE
		  SET etag = EXCLUDED.etag,
		      size_bytes = EXCLUDED.size_bytes,
		      uploaded_at = now()`,
		uploadID, partNumber, etag, n); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if _, err := tx.Exec(r.Context(),
		`UPDATE multipart_uploads SET total_size = total_size + $1 WHERE upload_id = $2`,
		n-oldSize, uploadID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "COMMIT", err.Error())
		return
	}

	success = true
	w.Header().Set("ETag", `"`+etag+`"`)
	w.WriteHeader(http.StatusOK)
}

// ---------- POST /:bucket/*?uploadId=X — complete --------------------------

// Complete is the HTTP entry point on the authenticated chain. It pulls the
// caller's user ID from the request context and delegates to CompleteForUser.
//
// The signed-URL multipart finalize path in share.go calls CompleteForUser
// directly with the upload's recorded owner_id — so the actual implementation
// never reads from the context user struct, avoiding the historical
// "synthesize a fake User" pattern.
func (h *MultipartHandler) Complete(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	h.CompleteForUser(w, r, u.ID)
}

// CompleteForUser does the actual work of finishing a multipart upload.
// userID MUST be the authenticated owner — either pulled from context
// (Complete) or resolved from a presigned-URL signature (share.go).
func (h *MultipartHandler) CompleteForUser(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	bucketName := chi.URLParam(r, "bucket")
	uploadID := r.URL.Query().Get("uploadId")

	bucketID, key, contentType, err := loadUploadByID(r.Context(), h.DB, uploadID, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_UPLOAD", "upload not found or not yours")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Parse client's part list. S3 sends XML; dashboard sends JSON.
	var req CompleteReq
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "xml") || isS3Client(r) {
		var x completeMultipartUploadRequest
		if err := xml.NewDecoder(r.Body).Decode(&x); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_XML", err.Error())
			return
		}
		for _, p := range x.Parts {
			req.Parts = append(req.Parts, PartInfo{PartNumber: p.PartNumber, ETag: p.ETag})
		}
	} else {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
			return
		}
	}
	if len(req.Parts) == 0 {
		httpx.WriteError(w, http.StatusBadRequest, "NO_PARTS", "parts list cannot be empty")
		return
	}
	sort.Slice(req.Parts, func(i, j int) bool {
		return req.Parts[i].PartNumber < req.Parts[j].PartNumber
	})

	// Verify each claimed part exists in DB with matching ETag.
	rows, err := h.DB.Query(r.Context(),
		`SELECT part_number, etag, size_bytes FROM multipart_parts WHERE upload_id = $1`,
		uploadID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	storedParts := make(map[int]PartInfo)
	for rows.Next() {
		var p PartInfo
		if err := rows.Scan(&p.PartNumber, &p.ETag, &p.Size); err != nil {
			rows.Close()
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		storedParts[p.PartNumber] = p
	}
	rows.Close()

	for i, p := range req.Parts {
		stored, ok := storedParts[p.PartNumber]
		if !ok {
			httpx.WriteError(w, http.StatusBadRequest, "MISSING_PART",
				fmt.Sprintf("part %d was never uploaded", p.PartNumber))
			return
		}
		clientETag := normalizeETag(p.ETag)
		if clientETag != stored.ETag {
			httpx.WriteError(w, http.StatusBadRequest, "ETAG_MISMATCH",
				fmt.Sprintf("part %d etag mismatch", p.PartNumber))
			return
		}
		// All parts except the last must be ≥ 5MB (S3 rule).
		if i < len(req.Parts)-1 && stored.Size < 5*1024*1024 {
			httpx.WriteError(w, http.StatusBadRequest, "PART_TOO_SMALL",
				fmt.Sprintf("part %d is %d bytes; minimum is 5 MiB except for the last part",
					p.PartNumber, stored.Size))
			return
		}
	}

	// Concatenate parts → atomic write into final object path.
	destPath := h.FS.ObjectPath(bucketID.String(), key)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "FS_MKDIR", err.Error())
		return
	}
	tmpPath := destPath + ".assembling"
	dst, err := os.Create(tmpPath)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "FS_CREATE", err.Error())
		return
	}

	overallHash := md5.New()
	var totalSize int64
	for _, p := range req.Parts {
		partPath := h.FS.MultipartPartPath(bucketID.String(), uploadID, p.PartNumber)
		src, err := os.Open(partPath)
		if err != nil {
			_ = dst.Close()
			_ = os.Remove(tmpPath)
			httpx.WriteError(w, http.StatusInternalServerError, "FS_OPEN_PART", err.Error())
			return
		}
		n, err := io.Copy(dst, src)
		_ = src.Close()
		if err != nil {
			_ = dst.Close()
			_ = os.Remove(tmpPath)
			httpx.WriteError(w, http.StatusInternalServerError, "FS_COPY", err.Error())
			return
		}
		totalSize += n

		// S3 multipart ETag = MD5(concat(MD5_bytes_of_each_part)) + "-N"
		partETagBytes, err := hex.DecodeString(storedParts[p.PartNumber].ETag)
		if err == nil {
			overallHash.Write(partETagBytes)
		}
	}
	if err := dst.Sync(); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmpPath)
		httpx.WriteError(w, http.StatusInternalServerError, "FS_SYNC", err.Error())
		return
	}
	_ = dst.Close()

	// If replacing existing object, refund its size from quota.
	var existingSize int64
	_ = h.DB.QueryRow(r.Context(),
		`SELECT size_bytes FROM objects WHERE bucket_id = $1 AND key = $2`,
		bucketID, key,
	).Scan(&existingSize)
	if existingSize > 0 {
		_ = middleware.QuotaReserve(r.Context(), h.DB, userID, -existingSize)
	}

	// Move assembled file into place (atomic).
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		// Refund quota we already gave back
		if existingSize > 0 {
			_ = middleware.QuotaReserve(r.Context(), h.DB, userID, existingSize)
		}
		httpx.WriteError(w, http.StatusInternalServerError, "FS_RENAME", err.Error())
		return
	}

	finalETag := fmt.Sprintf("%s-%d", hex.EncodeToString(overallHash.Sum(nil)), len(req.Parts))
	storagePath := h.FS.ObjectPath(bucketID.String(), key)

	// Detect whether this is a fresh insert (vs. overwrite) so we know whether
	// to bump the shard count.
	var existed bool
	_ = h.DB.QueryRow(r.Context(),
		`SELECT TRUE FROM objects WHERE bucket_id = $1 AND key = $2 AND NOT is_deleted`,
		bucketID, key).Scan(&existed)

	// Insert object row.
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
		bucketID, key, totalSize, finalETag, contentType, storagePath,
	).Scan(&objectID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if !existed {
		_ = sharding.OnObjectAdded(r.Context(), h.DB, bucketID, objectID, key)
	}

	enqueueTranscode(r.Context(), h.DB, h.FS, objectID, bucketID, key, contentType, r)

	if isS3Client(r) {
		writeXML(w, http.StatusOK, completeMultipartUploadResult{
			XMLNS:    s3XMLNS,
			Location: "/" + bucketName + "/" + key,
			Bucket:   bucketName,
			Key:      key,
			ETag:     `"` + finalETag + `"`,
		})
		return
	}

	// Mark upload complete and remove tmp dir + parts rows (cascade via FK).
	if _, err := h.DB.Exec(r.Context(),
		`UPDATE multipart_uploads SET status = 'complete' WHERE upload_id = $1`, uploadID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	_ = os.RemoveAll(h.FS.MultipartDir(bucketID.String(), uploadID))

	httpx.WriteJSON(w, http.StatusOK, CompleteResp{
		Bucket: bucketName, Key: key, ETag: finalETag, Size: totalSize,
	})
}

// ---------- DELETE /:bucket/*?uploadId=X — abort ---------------------------

// Abort is the HTTP entry point on the authenticated chain.
func (h *MultipartHandler) Abort(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	h.AbortForUser(w, r, u.ID)
}

// AbortForUser does the actual work. userID MUST be the authenticated owner.
func (h *MultipartHandler) AbortForUser(w http.ResponseWriter, r *http.Request, userID uuid.UUID) {
	uploadID := r.URL.Query().Get("uploadId")

	var bucketID uuid.UUID
	var totalSize int64
	err := h.DB.QueryRow(r.Context(), `
		SELECT bucket_id, total_size FROM multipart_uploads
		 WHERE upload_id = $1 AND owner_id = $2 AND status = 'in-progress'`,
		uploadID, userID,
	).Scan(&bucketID, &totalSize)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_UPLOAD", "upload not found or not yours")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Refund quota — use context.Background() because Abort is the LAST
	// thing the client does and r.Context() can be cancelled out from
	// under us by the same disconnect that triggered the abort.
	if totalSize > 0 {
		_ = middleware.QuotaReserve(context.Background(), h.DB, userID, -totalSize)
	}

	if _, err := h.DB.Exec(context.Background(),
		`UPDATE multipart_uploads SET status = 'aborted' WHERE upload_id = $1`, uploadID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	tmpDir := h.FS.MultipartDir(bucketID.String(), uploadID)
	if err := os.RemoveAll(tmpDir); err != nil {
		// Don't fail the request — DB row already marked aborted, so the
		// cleaner's sweepMultipartTmp will reap the dir on its next tick.
		// But log it so we know the safety net is doing real work.
		log.Printf("multipart abort: failed to remove %s: %v (cleaner will reap)", tmpDir, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- GET /:bucket/*?uploadId=X — list parts -------------------------

func (h *MultipartHandler) ListParts(w http.ResponseWriter, r *http.Request) {
	uploadID := r.URL.Query().Get("uploadId")
	_, _, _, err := loadUpload(r, h.DB, uploadID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_UPLOAD", "upload not found or not yours")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	rows, err := h.DB.Query(r.Context(),
		`SELECT part_number, etag, size_bytes FROM multipart_parts
		  WHERE upload_id = $1 ORDER BY part_number`, uploadID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	parts := []PartInfo{}
	for rows.Next() {
		var p PartInfo
		if err := rows.Scan(&p.PartNumber, &p.ETag, &p.Size); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		parts = append(parts, p)
	}
	httpx.WriteJSON(w, http.StatusOK, ListPartsResp{UploadID: uploadID, Parts: parts})
}

// ---------- GET /:bucket?uploads — list in-progress multipart uploads -----

// UploadEntry is one in-progress upload returned by ListUploads.
type UploadEntry struct {
	UploadID   string    `json:"uploadId"`
	Key        string    `json:"key"`
	TotalBytes int64     `json:"totalBytes"`
	NumParts   int       `json:"numParts"`
	CreatedAt  time.Time `json:"createdAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

type ListUploadsResp struct {
	Bucket  string        `json:"bucket"`
	Uploads []UploadEntry `json:"uploads"`
}

// ListUploads returns every in-progress multipart upload the caller owns
// in this bucket. Useful for recovering from a half-finished CLI upload
// (find the upload_id) or for ops triage of leaked reservations.
//
// JSON for native clients; XML (S3 ListMultipartUploads shape) for SDK
// clients via the isS3Client probe.
func (h *MultipartHandler) ListUploads(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	bucketName := chi.URLParam(r, "bucket")

	bucketID, err := resolveBucket(r, h.DB, bucketName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_BUCKET", "bucket not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	rows, err := h.DB.Query(r.Context(), `
		SELECT mu.upload_id, mu.key, mu.total_size, mu.created_at, mu.expires_at,
		       (SELECT COUNT(*) FROM multipart_parts mp WHERE mp.upload_id = mu.upload_id)
		  FROM multipart_uploads mu
		 WHERE mu.bucket_id = $1
		   AND mu.owner_id  = $2
		   AND mu.status    = 'in-progress'
		   AND mu.expires_at > now()
		 ORDER BY mu.created_at DESC`,
		bucketID, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := ListUploadsResp{Bucket: bucketName, Uploads: []UploadEntry{}}
	for rows.Next() {
		var e UploadEntry
		if err := rows.Scan(&e.UploadID, &e.Key, &e.TotalBytes,
			&e.CreatedAt, &e.ExpiresAt, &e.NumParts); err != nil {
			continue
		}
		out.Uploads = append(out.Uploads, e)
	}

	if isS3Client(r) {
		writeXML(w, http.StatusOK, listMultipartUploadsResult{
			XMLNS:  s3XMLNS,
			Bucket: bucketName,
			Uploads: func() []s3UploadXML {
				xml := make([]s3UploadXML, 0, len(out.Uploads))
				for _, e := range out.Uploads {
					xml = append(xml, s3UploadXML{
						Key:       e.Key,
						UploadID:  e.UploadID,
						Initiated: e.CreatedAt.UTC().Format(time.RFC3339),
					})
				}
				return xml
			}(),
		})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// normalizeETag strips surrounding quotes from an ETag value (S3 sends quoted ETags).
func normalizeETag(e string) string {
	if len(e) >= 2 && e[0] == '"' && e[len(e)-1] == '"' {
		return e[1 : len(e)-1]
	}
	return e
}

