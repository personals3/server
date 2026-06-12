package handlers

import (
	"bytes"
	"context"
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

// Regression test for the quota-reconciliation bug in PutObject: a
// non-quota DB error from the post-write adjustment was silently ignored —
// the request proceeded to commit the object row with quota accounting
// that never happened. The fixed handler must 500, remove the written
// file, refund the pre-reservation exactly, and leave no object row.
func TestPutObjectQuotaAdjustFailure(t *testing.T) {
	pool := testPool(t)
	fs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := &ObjectHandler{DB: pool, FS: fs}

	userID, bearer := newTestUser(t, pool, 1<<30)
	bucketID := newTestBucket(t, pool, userID, "regress-put")

	router := chi.NewRouter()
	router.Use(middleware.Authenticator(pool, "test-jwt-secret"))
	router.Put("/{bucket}/*", h.PutObject)

	base := usedBytes(t, pool, userID)

	orig := quotaAdjust
	quotaAdjust = func(ctx context.Context, db *pgxpool.Pool, id uuid.UUID, delta int64) error {
		return errors.New("synthetic db failure")
	}
	t.Cleanup(func() { quotaAdjust = orig })

	// Claim 100 bytes, send 150 → +50 adjustment hits the failing seam
	// after the object file is already on disk.
	body := bytes.Repeat([]byte("x"), 150)
	req := httptest.NewRequest("PUT", "/regress-put/file.bin", bytes.NewReader(body))
	req.Header.Set("Authorization", bearer)
	req.ContentLength = 100
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 500 {
		t.Fatalf("put object: want 500 on failed quota adjustment, got %d: %s",
			rec.Code, rec.Body.String())
	}
	if got := usedBytes(t, pool, userID); got != base {
		t.Errorf("used_bytes drifted: baseline %d, after failed put %d", base, got)
	}

	var objCount int
	if err := pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM objects WHERE bucket_id = $1 AND key = 'file.bin'`,
		bucketID,
	).Scan(&objCount); err != nil {
		t.Fatalf("count objects: %v", err)
	}
	if objCount != 0 {
		t.Errorf("want no object rows after failed put, got %d", objCount)
	}

	objPath := fs.ObjectPath(bucketID.String(), "file.bin")
	if _, err := os.Stat(objPath); !os.IsNotExist(err) {
		t.Errorf("object file still on disk at %s (err=%v)", objPath, err)
	}
}

// Sanity check the normal divergence path: actual size larger than
// Content-Length still lands with used_bytes charged for actual bytes.
func TestPutObjectSizeDivergenceCharged(t *testing.T) {
	pool := testPool(t)
	fs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := &ObjectHandler{DB: pool, FS: fs}

	userID, bearer := newTestUser(t, pool, 1<<30)
	newTestBucket(t, pool, userID, "regress-put2")

	router := chi.NewRouter()
	router.Use(middleware.Authenticator(pool, "test-jwt-secret"))
	router.Put("/{bucket}/*", h.PutObject)

	base := usedBytes(t, pool, userID)

	body := bytes.Repeat([]byte("x"), 150)
	req := httptest.NewRequest("PUT", "/regress-put2/file.bin", bytes.NewReader(body))
	req.Header.Set("Authorization", bearer)
	req.ContentLength = 100
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("put object: want 2xx, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := usedBytes(t, pool, userID); got != base+150 {
		t.Errorf("used_bytes: want baseline+150 (%d), got %d", base+150, got)
	}
}
