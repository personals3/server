package middleware

import (
	"context"
	"errors"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/storage"
	"github.com/personals3/api/internal/sysconfig"
)

// debugQuota is true when DEBUG_QUOTA=1 in the env. When on, every
// QuotaReserve call logs one line with caller + delta + new balance.
// Useful for race / drift investigations; noisy in production.
var debugQuota = os.Getenv("DEBUG_QUOTA") == "1"

// ErrQuotaExceeded indicates the operation would put the user above quota.
var ErrQuotaExceeded = errors.New("quota exceeded")

// QuotaSnapshot returns the user's current quota_bytes and used_bytes. Used
// to enrich quota-rejection errors with "you need N more bytes" details.
// Returns (0, 0, err) on any error.
func QuotaSnapshot(ctx context.Context, db *pgxpool.Pool, userID uuid.UUID) (quotaBytes, usedBytes int64, err error) {
	err = db.QueryRow(ctx,
		`SELECT quota_bytes, used_bytes FROM users WHERE id = $1`, userID,
	).Scan(&quotaBytes, &usedBytes)
	return
}

// QuotaErrorDetails builds the standard machine-readable error context for
// a quota-rejected request. `requested` is how many bytes the caller was
// trying to reserve.
func QuotaErrorDetails(ctx context.Context, db *pgxpool.Pool, userID uuid.UUID, requested int64) map[string]any {
	quota, used, err := QuotaSnapshot(ctx, db, userID)
	if err != nil {
		return map[string]any{"requestedBytes": requested}
	}
	available := quota - used
	if available < 0 {
		available = 0
	}
	deficit := requested - available
	if deficit < 0 {
		deficit = 0
	}
	return map[string]any{
		"requestedBytes": requested,
		"usedBytes":      used,
		"quotaBytes":     quota,
		"availableBytes": available,
		"deficitBytes":   deficit,
	}
}

// ErrDiskFull indicates the physical disk has crossed the configured
// fullness threshold — reject new writes regardless of per-user quota.
var ErrDiskFull = errors.New("disk full")

// CheckDiskHealthy returns ErrDiskFull if the storage disk is past the
// configured "disk_full_threshold_pct". Call before accepting an upload.
// Caches nothing — it's a single statfs(2) syscall, ~µs cost.
func CheckDiskHealthy(ctx context.Context, db *pgxpool.Pool, storageRoot string) error {
	d, err := storage.Stat(storageRoot)
	if err != nil {
		return nil // can't check, allow
	}
	threshold := sysconfig.GetInt64(ctx, db, sysconfig.KeyDiskFullThresholdPct, 95)
	if threshold <= 0 || threshold >= 100 {
		return nil
	}
	if d.Total == 0 {
		return nil
	}
	usedPct := (d.Used * 100) / d.Total
	if usedPct >= uint64(threshold) {
		return ErrDiskFull
	}
	return nil
}

// QuotaReserve atomically attempts to add `delta` bytes to a user's used_bytes.
// Returns ErrQuotaExceeded if it would put them over their quota.
//
// Caller should refund (call with negative delta) on failure paths.
// delta may be negative — that is, this is also used to refund on delete.
func QuotaReserve(ctx context.Context, db *pgxpool.Pool, userID uuid.UUID, delta int64) error {
	if delta == 0 {
		return nil
	}
	caller := quotaCaller(2)
	if delta < 0 {
		var newBytes int64
		err := db.QueryRow(ctx, `
			UPDATE users
			   SET used_bytes = GREATEST(used_bytes + $1, 0)
			 WHERE id = $2
			 RETURNING used_bytes`, delta, userID).Scan(&newBytes)
		if debugQuota {
			log.Printf("QUOTA user=%s delta=%+d new=%d via=%s err=%v",
				userID, delta, newBytes, caller, err)
		}
		broadcastQuotaEvent(QuotaEvent{TS: time.Now(), UserID: userID,
			Delta: delta, NewBytes: newBytes, Caller: caller})
		return err
	}

	var newBytes int64
	err := db.QueryRow(ctx, `
		UPDATE users
		   SET used_bytes = used_bytes + $1
		 WHERE id = $2
		   AND used_bytes + $1 <= quota_bytes
		 RETURNING used_bytes`,
		delta, userID,
	).Scan(&newBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		if debugQuota {
			log.Printf("QUOTA user=%s delta=%+d REJECTED via=%s",
				userID, delta, caller)
		}
		broadcastQuotaEvent(QuotaEvent{TS: time.Now(), UserID: userID,
			Delta: delta, Rejected: true, Caller: caller})
		return ErrQuotaExceeded
	}
	if debugQuota {
		log.Printf("QUOTA user=%s delta=%+d new=%d via=%s err=%v",
			userID, delta, newBytes, caller, err)
	}
	broadcastQuotaEvent(QuotaEvent{TS: time.Now(), UserID: userID,
		Delta: delta, NewBytes: newBytes, Caller: caller})
	return err
}

func quotaCaller(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "?"
	}
	short := file
	if i := lastSlash(file); i >= 0 {
		short = file[i+1:]
	}
	return short + ":" + itoa(line)
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
