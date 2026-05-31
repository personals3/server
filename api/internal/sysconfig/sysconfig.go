// Package sysconfig wraps the system_config key/value table.
// Used for runtime tunables that an admin can change without redeploying.
package sysconfig

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	KeyTotalQuotaBytes      = "total_quota_bytes"
	KeyReservedBytes        = "reserved_bytes"
	KeyDiskFullThresholdPct = "disk_full_threshold_pct"
	KeyOvercommitAllowed    = "overcommit_allowed"

	// Cleaner runtime tunables — overrideable per row in system_config so
	// admins don't need to edit .env + rebuild the container to flip them.
	// Each falls back to its env-file default if the row is missing.
	KeyCleanupDryRun           = "cleanup_dry_run"            // bool
	KeyOrphanTwoStrike         = "orphan_two_strike"          // bool
	KeyOrphanMinAgeMinutes     = "orphan_min_age_minutes"     // int64 minutes
	KeyBloomRebuildHours       = "bloom_rebuild_hours"        // int64 hours
	KeyCleanupIntervalSeconds  = "cleanup_interval_seconds"   // int64 seconds

	// Capacity caps — admin-tunable, no rebuild.
	//   max_users: maximum active accounts. 0 = unlimited.
	//   max_allocation_pct: percentage of effective disk that SUM(user
	//     quotas) may consume. overcommit_allowed bypasses.
	KeyMaxUsers          = "max_users"           // int64 (0 = unlimited)
	KeyMaxAllocationPct  = "max_allocation_pct"  // int64 1-100
)

type Entry struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Description string `json:"description"`
}

func Get(ctx context.Context, db *pgxpool.Pool, key string) (string, error) {
	var v string
	err := db.QueryRow(ctx,
		`SELECT value FROM system_config WHERE key = $1`, key,
	).Scan(&v)
	return v, err
}

func GetInt64(ctx context.Context, db *pgxpool.Pool, key string, def int64) int64 {
	v, err := Get(ctx, db, key)
	if err != nil {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func GetBool(ctx context.Context, db *pgxpool.Pool, key string, def bool) bool {
	v, err := Get(ctx, db, key)
	if err != nil {
		return def
	}
	return v == "true" || v == "1" || v == "yes"
}

func Set(ctx context.Context, db *pgxpool.Pool, key, value string) error {
	tag, err := db.Exec(ctx,
		`UPDATE system_config SET value = $2, updated_at = now() WHERE key = $1`,
		key, value)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

func All(ctx context.Context, db *pgxpool.Pool) ([]Entry, error) {
	rows, err := db.Query(ctx,
		`SELECT key, value, COALESCE(description, '') FROM system_config ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Key, &e.Value, &e.Description); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

