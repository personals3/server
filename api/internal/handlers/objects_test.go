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

// putObject PUTs n bytes of body to path, claiming `claim` bytes via
// Content-Length (pass int64(n) for an accurate claim).
func putObject(t *testing.T, router *chi.Mux, bearer, path string, n int, claim int64) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("PUT", path, bytes.NewReader(bytes.Repeat([]byte("x"), n)))
	req.Header.Set("Authorization", bearer)
	req.ContentLength = claim
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Regression test for the shrinking-overwrite quota leak: with versioning
// off, overwriting a 100-byte object with 40 bytes computed a negative
// reservation that was never applied, yet the reconciliation compared
// against it as if it had been — so the 60 freed bytes stayed charged
// until quota-reconcile.sql.
func TestPutObjectShrinkingOverwriteRefunds(t *testing.T) {
	pool := testPool(t)
	fs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := &ObjectHandler{DB: pool, FS: fs}

	userID, bearer := newTestUser(t, pool, 1<<30)
	bucketID := newTestBucket(t, pool, userID, "regress-shrink")

	router := chi.NewRouter()
	router.Use(middleware.Authenticator(pool, "test-jwt-secret"))
	router.Put("/{bucket}/*", h.PutObject)

	base := usedBytes(t, pool, userID)

	if rec := putObject(t, router, bearer, "/regress-shrink/file.bin", 100, 100); rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("first put: got %d: %s", rec.Code, rec.Body.String())
	}
	if got := usedBytes(t, pool, userID); got != base+100 {
		t.Fatalf("after first put: want %d, got %d", base+100, got)
	}

	if rec := putObject(t, router, bearer, "/regress-shrink/file.bin", 40, 40); rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("shrinking put: got %d: %s", rec.Code, rec.Body.String())
	}
	if got := usedBytes(t, pool, userID); got != base+40 {
		t.Errorf("freed bytes not refunded: want %d, got %d", base+40, got)
	}

	var sizeOnRow int64
	if err := pool.QueryRow(context.Background(),
		`SELECT size_bytes FROM objects WHERE bucket_id = $1 AND key = 'file.bin'`,
		bucketID,
	).Scan(&sizeOnRow); err != nil {
		t.Fatalf("read object row: %v", err)
	}
	if sizeOnRow != 40 {
		t.Errorf("object row size: want 40, got %d", sizeOnRow)
	}
}

// Same shrink but with the body diverging from Content-Length (claim 50,
// send 70 over a 100-byte object): the old adjustment math computed
// against the unapplied negative reservation and CHARGED 20 instead of
// refunding 30.
func TestPutObjectShrinkWithDivergenceRefunds(t *testing.T) {
	pool := testPool(t)
	fs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := &ObjectHandler{DB: pool, FS: fs}

	userID, bearer := newTestUser(t, pool, 1<<30)
	newTestBucket(t, pool, userID, "regress-shrink2")

	router := chi.NewRouter()
	router.Use(middleware.Authenticator(pool, "test-jwt-secret"))
	router.Put("/{bucket}/*", h.PutObject)

	base := usedBytes(t, pool, userID)

	if rec := putObject(t, router, bearer, "/regress-shrink2/file.bin", 100, 100); rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("first put: got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := putObject(t, router, bearer, "/regress-shrink2/file.bin", 70, 50); rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("diverging shrink put: got %d: %s", rec.Code, rec.Body.String())
	}
	if got := usedBytes(t, pool, userID); got != base+70 {
		t.Errorf("used_bytes: want actual size %d, got %d", base+70, got)
	}
}

// With versioning ON the old bytes stay on disk as a snapshot, so a
// shrinking overwrite must NOT refund anything: 100-byte version + 40-byte
// current = 140 charged.
func TestPutObjectShrinkingOverwriteVersioningKeepsCharge(t *testing.T) {
	pool := testPool(t)
	fs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := &ObjectHandler{DB: pool, FS: fs}

	userID, bearer := newTestUser(t, pool, 1<<30)
	bucketID := newTestBucket(t, pool, userID, "regress-shrink-ver")
	if _, err := pool.Exec(context.Background(),
		`UPDATE buckets SET versioning = true WHERE id = $1`, bucketID); err != nil {
		t.Fatalf("enable versioning: %v", err)
	}

	router := chi.NewRouter()
	router.Use(middleware.Authenticator(pool, "test-jwt-secret"))
	router.Put("/{bucket}/*", h.PutObject)

	base := usedBytes(t, pool, userID)

	if rec := putObject(t, router, bearer, "/regress-shrink-ver/file.bin", 100, 100); rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("first put: got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := putObject(t, router, bearer, "/regress-shrink-ver/file.bin", 40, 40); rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("versioned shrink put: got %d: %s", rec.Code, rec.Body.String())
	}

	if got := usedBytes(t, pool, userID); got != base+140 {
		t.Errorf("used_bytes: want version+current (%d), got %d", base+140, got)
	}

	var versionCount int
	var versionBytes int64
	if err := pool.QueryRow(context.Background(), `
		SELECT COUNT(*), COALESCE(SUM(ov.size_bytes), 0)
		  FROM object_versions ov
		  JOIN objects o ON o.id = ov.object_id
		 WHERE o.bucket_id = $1 AND o.key = 'file.bin'`,
		bucketID,
	).Scan(&versionCount, &versionBytes); err != nil {
		t.Fatalf("read versions: %v", err)
	}
	if versionCount != 1 || versionBytes != 100 {
		t.Errorf("versions: want 1 row / 100 bytes, got %d rows / %d bytes",
			versionCount, versionBytes)
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
