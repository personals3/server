package cleaner

// DB-backed test for the quota self-heal. Same contract as the handlers
// package tests: needs a THROWAWAY Postgres, skipped unless
// TEST_DATABASE_URL is set (the helper DROPs the public schema):
//
//   docker run --rm -d --name ps3-test-pg -p 55432:5432 \
//     -e POSTGRES_PASSWORD=test postgres:16-alpine
//   TEST_DATABASE_URL=postgres://postgres:test@localhost:55432/postgres \
//     go test ./internal/cleaner/

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB-backed test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	files, err := filepath.Glob("../../../db/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return pool
}

// seedUser creates a user with `recorded` as their (possibly wrong)
// used_bytes plus: one 100-byte object carrying a 40-byte transcode output
// and a 25-byte reservation, one 50-byte prior version, and an in-progress
// multipart upload holding 30 bytes. True usage = 245 bytes.
const seededActual = 100 + 40 + 25 + 50 + 30

func seedUser(t *testing.T, pool *pgxpool.Pool, recorded int64) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var userID, bucketID, objectID uuid.UUID
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, name, quota_bytes, used_bytes)
		VALUES ($1, 'Reconcile Test', 1073741824, $2) RETURNING id`,
		fmt.Sprintf("test-%s@example.test", uuid.NewString()), recorded,
	).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO buckets (name, owner_id) VALUES ($1, $2) RETURNING id`,
		fmt.Sprintf("rq-%s", uuid.NewString()[:8]), userID,
	).Scan(&bucketID); err != nil {
		t.Fatalf("insert bucket: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO objects (bucket_id, key, size_bytes, etag, storage_path,
		                     transcoded_bytes, transcode_reserved_bytes)
		VALUES ($1, 'a.mp4', 100, 'e', '/x', 40, 25) RETURNING id`,
		bucketID,
	).Scan(&objectID); err != nil {
		t.Fatalf("insert object: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO object_versions (object_id, version_id, size_bytes, etag, content_type, storage_path)
		VALUES ($1, 'v1', 50, 'e2', 'video/mp4', '/x/v1')`, objectID); err != nil {
		t.Fatalf("insert version: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO multipart_uploads (upload_id, bucket_id, key, owner_id, content_type, total_size)
		VALUES ($1, $2, 'big.bin', $3, 'application/octet-stream', 30)`,
		uuid.NewString(), bucketID, userID); err != nil {
		t.Fatalf("insert multipart: %v", err)
	}
	return userID
}

func usedBytes(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT used_bytes FROM users WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read used_bytes: %v", err)
	}
	return n
}

func runReconcile(t *testing.T, pool *pgxpool.Pool, dryRun bool) *runRecord {
	t.Helper()
	logFile, err := os.Create(filepath.Join(t.TempDir(), "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	c := &Cleaner{
		cfg: Config{DryRun: dryRun, QuotaDriftThreshold: 1024},
		db:  pool,
	}
	rec := &runRecord{Counts: map[string]int{}, log: logFile, dryRun: dryRun}
	if err := c.reconcileQuotas(context.Background(), rec); err != nil {
		t.Fatalf("reconcileQuotas: %v", err)
	}
	return rec
}

// Drifted users get repaired to the full recomputed total — including the
// in-flight multipart bytes the manual script misses — while users within
// the threshold are left untouched.
func TestReconcileQuotasRepairsDrift(t *testing.T) {
	pool := testPool(t)

	drifted := seedUser(t, pool, 999_999) // way off
	closeEnough := seedUser(t, pool, seededActual+512) // within 1 KiB threshold

	rec := runReconcile(t, pool, false)

	if got := usedBytes(t, pool, drifted); got != seededActual {
		t.Errorf("drifted user: want used_bytes %d (incl. multipart term), got %d",
			seededActual, got)
	}
	if got := usedBytes(t, pool, closeEnough); got != seededActual+512 {
		t.Errorf("within-threshold user touched: want %d, got %d",
			seededActual+512, got)
	}
	if rec.Counts["quota_reconcile"] != 1 {
		t.Errorf("want exactly 1 correction audited, got %d", rec.Counts["quota_reconcile"])
	}
}

// Dry-run reports drift but changes nothing.
func TestReconcileQuotasDryRun(t *testing.T) {
	pool := testPool(t)
	drifted := seedUser(t, pool, 999_999)

	rec := runReconcile(t, pool, true)

	if got := usedBytes(t, pool, drifted); got != 999_999 {
		t.Errorf("dry-run mutated used_bytes: %d", got)
	}
	if rec.Counts["quota_reconcile"] != 1 {
		t.Errorf("dry-run should still audit the drifted user, got %d",
			rec.Counts["quota_reconcile"])
	}
}
