package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/storage"
)

// Regression test for the quota-reconciliation bug in UploadPart: when the
// post-write adjustment (actual bytes vs Content-Length) failed with a
// non-quota DB error, the old code swallowed the error and folded the
// never-applied adjustment into `reserved` anyway — so the cleanup refund
// returned bytes that were never charged and the user's quota drifted.
//
// The fixed handler must 500, refund the original reservation exactly,
// and leave no part row or on-disk part file behind.
func TestUploadPartQuotaAdjustFailure(t *testing.T) {
	pool := testPool(t)
	fs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := &MultipartHandler{DB: pool, FS: fs}

	userID, bearer := newTestUser(t, pool, 1<<30) // 1 GiB quota
	bucketID := newTestBucket(t, pool, userID, "regress-mp")

	// Same mount shape as cmd/server/main.go: auth middleware + bucket/key
	// route params; multipart endpoints dispatch on query string.
	router := chi.NewRouter()
	router.Use(middleware.Authenticator(pool, "test-jwt-secret"))
	router.Post("/{bucket}/*", h.Initiate)
	router.Put("/{bucket}/*", h.UploadPart)

	// Initiate an upload.
	req := httptest.NewRequest("POST", "/regress-mp/big.bin?uploads", nil)
	req.Header.Set("Authorization", bearer)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("initiate: got %d: %s", rec.Code, rec.Body.String())
	}
	var initResp InitiateResp
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("initiate response: %v", err)
	}

	base := usedBytes(t, pool, userID)

	// Make the reconciliation step fail with a synthetic non-quota DB error.
	orig := quotaAdjust
	quotaAdjust = func(ctx context.Context, db *pgxpool.Pool, id uuid.UUID, delta int64) error {
		return errors.New("synthetic db failure")
	}
	t.Cleanup(func() { quotaAdjust = orig })

	// Claim 100 bytes via Content-Length but send 150 → adjustment of +50
	// hits the failing seam after the part file is already on disk.
	body := bytes.Repeat([]byte("x"), 150)
	req = httptest.NewRequest("PUT",
		"/regress-mp/big.bin?uploadId="+initResp.UploadID+"&partNumber=1",
		bytes.NewReader(body))
	req.Header.Set("Authorization", bearer)
	req.ContentLength = 100
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("upload part: want 500 on failed quota adjustment, got %d: %s",
			rec.Code, rec.Body.String())
	}
	if got := usedBytes(t, pool, userID); got != base {
		t.Errorf("used_bytes drifted: baseline %d, after failed part upload %d", base, got)
	}

	var partCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM multipart_parts WHERE upload_id = $1`,
		initResp.UploadID,
	).Scan(&partCount); err != nil {
		t.Fatalf("count parts: %v", err)
	}
	if partCount != 0 {
		t.Errorf("want no part rows after failed upload, got %d", partCount)
	}

	partPath := fs.MultipartPartPath(bucketID.String(), initResp.UploadID, 1)
	if _, err := os.Stat(partPath); !os.IsNotExist(err) {
		t.Errorf("part file still on disk at %s (err=%v)", partPath, err)
	}
}

// Sanity check that the seam doesn't break the normal path: a part whose
// actual size diverges from Content-Length still lands, with used_bytes
// charged for the actual bytes.
func TestUploadPartSizeDivergenceCharged(t *testing.T) {
	pool := testPool(t)
	fs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := &MultipartHandler{DB: pool, FS: fs}

	userID, bearer := newTestUser(t, pool, 1<<30)
	newTestBucket(t, pool, userID, "regress-mp2")

	router := chi.NewRouter()
	router.Use(middleware.Authenticator(pool, "test-jwt-secret"))
	router.Post("/{bucket}/*", h.Initiate)
	router.Put("/{bucket}/*", h.UploadPart)

	req := httptest.NewRequest("POST", "/regress-mp2/big.bin?uploads", nil)
	req.Header.Set("Authorization", bearer)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("initiate: got %d: %s", rec.Code, rec.Body.String())
	}
	var initResp InitiateResp
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("initiate response: %v", err)
	}

	base := usedBytes(t, pool, userID)

	body := bytes.Repeat([]byte("x"), 150)
	req = httptest.NewRequest("PUT",
		"/regress-mp2/big.bin?uploadId="+initResp.UploadID+"&partNumber=1",
		bytes.NewReader(body))
	req.Header.Set("Authorization", bearer)
	req.ContentLength = 100
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("upload part: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := usedBytes(t, pool, userID); got != base+150 {
		t.Errorf("used_bytes: want baseline+150 (%d), got %d", base+150, got)
	}
}
