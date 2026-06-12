package handlers

// DB-backed regression-test helpers. These tests need a throwaway Postgres
// and are skipped unless TEST_DATABASE_URL is set:
//
//   docker run --rm -d --name ps3-test-pg -p 55432:5432 \
//     -e POSTGRES_PASSWORD=test postgres:16-alpine
//   TEST_DATABASE_URL=postgres://postgres:test@localhost:55432/postgres \
//     go test ./internal/handlers/
//
// The helper DROPs and recreates the public schema, then applies
// db/migrations/*.sql in order — never point TEST_DATABASE_URL at a real
// database. Tests authenticate through the real Authenticator middleware
// with a real API key (middleware deliberately exposes no context
// injection — see the WithUser note in middleware/auth.go).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/auth"
)

var (
	testPoolOnce sync.Once
	testPoolPg   *pgxpool.Pool
	testPoolErr  error
)

// testPool returns a pool connected to TEST_DATABASE_URL with a freshly
// migrated schema, shared across tests in this package. Skips the calling
// test when the env var is unset.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping DB-backed regression test")
	}
	testPoolOnce.Do(func() {
		ctx := context.Background()
		testPoolPg, testPoolErr = pgxpool.New(ctx, url)
		if testPoolErr != nil {
			return
		}
		if _, testPoolErr = testPoolPg.Exec(ctx,
			`DROP SCHEMA public CASCADE; CREATE SCHEMA public`); testPoolErr != nil {
			return
		}
		testPoolErr = applyMigrations(ctx, testPoolPg)
	})
	if testPoolErr != nil {
		t.Fatalf("test database setup: %v", testPoolErr)
	}
	return testPoolPg
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := filepath.Glob("../../../db/migrations/*.sql")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no migration files found relative to internal/handlers")
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	return nil
}

// newTestUser inserts a user with the given quota plus an API key, and
// returns the user ID and an Authorization header value that the real
// Authenticator accepts.
func newTestUser(t *testing.T, pool *pgxpool.Pool, quotaBytes int64) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()

	var userID uuid.UUID
	email := "test-" + uuid.NewString() + "@example.test"
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (email, name, quota_bytes)
		VALUES ($1, 'Regression Test', $2)
		RETURNING id`, email, quotaBytes,
	).Scan(&userID); err != nil {
		t.Fatalf("insert test user: %v", err)
	}

	plaintext, prefix, err := auth.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generate api key: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO api_keys (user_id, key_prefix, key_hash, name)
		VALUES ($1, $2, $3, 'regression')`,
		userID, prefix, auth.HashAPIKey(plaintext)); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return userID, "Bearer " + plaintext
}

// newTestBucket inserts a bucket owned by userID.
func newTestBucket(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID, name string) uuid.UUID {
	t.Helper()
	var bucketID uuid.UUID
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO buckets (name, owner_id) VALUES ($1, $2) RETURNING id`,
		name, userID,
	).Scan(&bucketID); err != nil {
		t.Fatalf("insert test bucket: %v", err)
	}
	return bucketID
}

func usedBytes(t *testing.T, pool *pgxpool.Pool, userID uuid.UUID) int64 {
	t.Helper()
	var used int64
	if err := pool.QueryRow(context.Background(),
		`SELECT used_bytes FROM users WHERE id = $1`, userID,
	).Scan(&used); err != nil {
		t.Fatalf("read used_bytes: %v", err)
	}
	return used
}
