package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/storage"
)

type listResp struct {
	Objects        []ObjectDTO `json:"objects"`
	CommonPrefixes []string    `json:"commonPrefixes"`
	Total          int         `json:"total"`
	Page           int         `json:"page"`
	Limit          int         `json:"limit"`
	PageCount      int         `json:"pageCount"`
	Truncated      bool        `json:"truncated"`
}

func seedObject(t *testing.T, pool *pgxpool.Pool, bucketID uuid.UUID, key string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO objects (bucket_id, key, size_bytes, etag, storage_path)
		VALUES ($1, $2, 10, 'e', '/x')`, bucketID, key); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
}

func listObjects(t *testing.T, router *chi.Mux, bearer, query string) listResp {
	t.Helper()
	req := httptest.NewRequest("GET", "/demo?"+query, nil)
	req.Header.Set("Authorization", bearer)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list ?%s: got %d: %s", query, rec.Code, rec.Body.String())
	}
	var out listResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestListObjectsFolderViewPagingSearch(t *testing.T) {
	pool := testPool(t)
	fs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	h := &ObjectHandler{DB: pool, FS: fs}
	userID, bearer := newTestUser(t, pool, 1<<30)
	bucketID := newTestBucket(t, pool, userID, "demo")

	// Layout: two folders, five direct files at root, a hidden marker.
	seedObject(t, pool, bucketID, "alpha/1.txt")
	seedObject(t, pool, bucketID, "alpha/2.txt")
	seedObject(t, pool, bucketID, "beta/report-2024.pdf")
	seedObject(t, pool, bucketID, ".keep") // hidden marker — must never surface
	for _, k := range []string{"root-a.txt", "root-b.txt", "root-c.txt", "report-root.txt", "root-e.txt"} {
		seedObject(t, pool, bucketID, k)
	}

	router := chi.NewRouter()
	router.Use(middleware.Authenticator(pool, "test-jwt-secret"))
	router.Get("/{bucket}", h.ListObjects)

	// --- Folder view, page 1 of 2 (limit 3) -------------------------------
	p1 := listObjects(t, router, bearer, "delimiter=%2F&limit=3&page=1")
	if got := len(p1.CommonPrefixes); got != 2 {
		t.Errorf("folders: want 2 (alpha/, beta/), got %d: %v", got, p1.CommonPrefixes)
	}
	if p1.Total != 5 {
		t.Errorf("total direct files: want 5 (hidden .keep excluded), got %d", p1.Total)
	}
	if p1.PageCount != 2 || len(p1.Objects) != 3 || !p1.Truncated {
		t.Errorf("page1: want 3 objects, pageCount 2, truncated; got %d objects pageCount %d truncated %v",
			len(p1.Objects), p1.PageCount, p1.Truncated)
	}

	// --- Folder view, page 2 (the remainder) ------------------------------
	p2 := listObjects(t, router, bearer, "delimiter=%2F&limit=3&page=2")
	if len(p2.Objects) != 2 || p2.Truncated {
		t.Errorf("page2: want 2 objects, not truncated; got %d truncated %v",
			len(p2.Objects), p2.Truncated)
	}
	// No overlap between pages.
	seen := map[string]bool{}
	for _, o := range append(append([]ObjectDTO{}, p1.Objects...), p2.Objects...) {
		if seen[o.Key] {
			t.Errorf("key %s appeared on both pages", o.Key)
		}
		seen[o.Key] = true
	}

	// --- Search: recursive, flat, crosses folders -------------------------
	s := listObjects(t, router, bearer, "delimiter=%2F&search=report")
	if len(s.CommonPrefixes) != 0 {
		t.Errorf("search mode should return no folders, got %v", s.CommonPrefixes)
	}
	// "report" matches beta/report-2024.pdf and report-root.txt.
	if s.Total != 2 {
		t.Errorf("search 'report': want 2 matches across folders, got %d (%v)", s.Total, keysOf(s.Objects))
	}

	// --- Hidden file never appears anywhere -------------------------------
	all := listObjects(t, router, bearer, "limit=1000")
	for _, o := range all.Objects {
		if o.Key == ".keep" {
			t.Error(".keep hidden marker leaked into flat listing")
		}
	}
}

func keysOf(objs []ObjectDTO) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Key
	}
	return out
}
