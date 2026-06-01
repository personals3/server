package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/personals3/api/internal/auth"
	"github.com/personals3/api/internal/cache"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
	"github.com/personals3/api/internal/storage"
	"github.com/personals3/api/internal/sysconfig"
)

type AdminHandler struct {
	DB          *pgxpool.Pool
	RDB         *redis.Client
	FS          *storage.FS
	StorageRoot string
}

// ============================================================================
// USERS
// ============================================================================

type AdminUserDTO struct {
	ID         uuid.UUID `json:"id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	QuotaBytes int64     `json:"quotaBytes"`
	UsedBytes  int64     `json:"usedBytes"`
	IsActive   bool      `json:"isActive"`
	CreatedAt  time.Time `json:"createdAt"`
}

// GET /admin/users
func (h *AdminHandler) ListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, email, name, role, quota_bytes, used_bytes, is_active, created_at
		  FROM users ORDER BY created_at DESC`)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []AdminUserDTO{}
	for rows.Next() {
		var u AdminUserDTO
		if err := rows.Scan(&u.ID, &u.Email, &u.Name, &u.Role,
			&u.QuotaBytes, &u.UsedBytes, &u.IsActive, &u.CreatedAt); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, u)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"users": out})
}

type createUserReq struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	Password   string `json:"password"`
	Role       string `json:"role"`       // "user" | "admin"
	QuotaBytes int64  `json:"quotaBytes"` // 0 = use default
}

// POST /admin/users
func (h *AdminHandler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	if req.Email == "" || req.Name == "" || req.Password == "" {
		httpx.WriteError(w, http.StatusBadRequest, "MISSING_FIELDS",
			"email, name, and password are required")
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}
	if req.Role != "admin" && req.Role != "user" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ROLE", "role must be 'admin' or 'user'")
		return
	}
	if req.QuotaBytes <= 0 {
		req.QuotaBytes = 10737418240 // 10 GB default
	}

	// Capacity check — enforces BOTH the user-count cap and the
	// allocation cap (90% by default) in one helper. overcommit_allowed
	// bypasses the allocation side only — the user-count cap always
	// applies because it's about real resource exhaustion (DB rows,
	// per-user bookkeeping), not promised bytes.
	if cap, err := CapacityHere(r.Context(), h.DB, h.StorageRoot); err == nil {
		if err := cap.CanAdmitNewUser(req.QuotaBytes); err != nil {
			code := "CAPACITY_FULL"
			if errors.Is(err, ErrOverAllocation) {
				code = "OVER_ALLOCATION"
			}
			httpx.WriteErrorDetails(w, http.StatusConflict, code, err.Error(),
				map[string]any{
					"userCount":      cap.UserCount,
					"maxUsers":       cap.MaxUsers,
					"allocatedBytes": cap.AllocatedBytes,
					"requestedBytes": req.QuotaBytes,
					"allocationCap":  cap.AllocationCap(),
				})
			return
		}
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "HASH", err.Error())
		return
	}

	var u AdminUserDTO
	err = h.DB.QueryRow(r.Context(), `
		INSERT INTO users (email, name, password_hash, role, quota_bytes)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, email, name, role, quota_bytes, used_bytes, is_active, created_at`,
		req.Email, req.Name, hash, req.Role, req.QuotaBytes,
	).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.QuotaBytes,
		&u.UsedBytes, &u.IsActive, &u.CreatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			httpx.WriteError(w, http.StatusConflict, "EMAIL_EXISTS", "a user with this email already exists")
			return
		}
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, u)
}

type updateUserReq struct {
	QuotaBytes *int64  `json:"quotaBytes,omitempty"`
	Role       *string `json:"role,omitempty"`
	IsActive   *bool   `json:"isActive,omitempty"`
	Name       *string `json:"name,omitempty"`
}

// PATCH /admin/users/:id
//
// IMPORTANT: this is the ONLY endpoint that may mutate users.quota_bytes
// or users.role. No user-facing PATCH exists for the user row. If a
// "PATCH /auth/me" is ever added, it MUST whitelist fields and reject
// quotaBytes / role / is_active / email. The quota-request flow is the
// only path by which a non-admin can move toward more space, and even
// that ends here, going through admin approval.
func (h *AdminHandler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid uuid")
		return
	}

	var req updateUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}

	if req.QuotaBytes != nil && *req.QuotaBytes < 0 {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_QUOTA", "quota must be ≥ 0")
		return
	}

	// Allocation cap check — only when quota is being changed and only
	// when the new value is larger (shrinking always allowed).
	if req.QuotaBytes != nil {
		var currentQuota int64
		_ = h.DB.QueryRow(r.Context(),
			`SELECT quota_bytes FROM users WHERE id = $1`, id,
		).Scan(&currentQuota)
		delta := *req.QuotaBytes - currentQuota
		if delta > 0 {
			cap, _ := CapacityHere(r.Context(), h.DB, h.StorageRoot)
			if err := cap.CanGrantQuotaDelta(delta); err != nil {
				httpx.WriteErrorDetails(w, http.StatusConflict, "OVER_ALLOCATION",
					err.Error(),
					map[string]any{
						"currentBytes":      currentQuota,
						"requestedBytes":    *req.QuotaBytes,
						"deltaBytes":        delta,
						"allocatedBytes":    cap.AllocatedBytes,
						"allocationCap":     cap.AllocationCap(),
						"maxAllocationPct":  cap.MaxAllocPct,
					})
				return
			}
		}
	}
	if req.Role != nil && *req.Role != "admin" && *req.Role != "user" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ROLE", "role must be 'admin' or 'user'")
		return
	}

	// Single UPDATE with COALESCE — keeps unset fields untouched.
	var u AdminUserDTO
	err = h.DB.QueryRow(r.Context(), `
		UPDATE users SET
		  quota_bytes = COALESCE($2, quota_bytes),
		  role        = COALESCE($3, role),
		  is_active   = COALESCE($4, is_active),
		  name        = COALESCE($5, name),
		  updated_at  = now()
		WHERE id = $1
		RETURNING id, email, name, role, quota_bytes, used_bytes, is_active, created_at`,
		id, req.QuotaBytes, req.Role, req.IsActive, req.Name,
	).Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.QuotaBytes,
		&u.UsedBytes, &u.IsActive, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, u)
}

// DELETE /admin/users/:id — soft delete via is_active=false.
func (h *AdminHandler) DeactivateUser(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid uuid")
		return
	}
	tag, err := h.DB.Exec(r.Context(),
		`UPDATE users SET is_active = false WHERE id = $1`, id)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusNotFound, "NOT_FOUND", "user not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ============================================================================
// AUDIT LOG
// ============================================================================

type AuditDTO struct {
	ID         int64     `json:"id"`
	UserID     *string   `json:"userId,omitempty"`
	UserEmail  *string   `json:"userEmail,omitempty"`
	Action     string    `json:"action"`
	Bucket     *string   `json:"bucket,omitempty"`
	Key        *string   `json:"key,omitempty"`
	SizeBytes  *int64    `json:"sizeBytes,omitempty"`
	StatusCode *int      `json:"statusCode,omitempty"`
	IPAddress  *string   `json:"ipAddress,omitempty"`
	Timestamp  time.Time `json:"ts"`
}

// GET /admin/audit?user=&action=&since=&until=&limit=&offset=
func (h *AdminHandler) ListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := 100
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	// Build dynamic WHERE — pgx parameter binding for safety.
	args := []any{}
	where := "TRUE"
	idx := 0
	add := func(clause string, val any) {
		idx++
		where += " AND " + clause + " $" + strconv.Itoa(idx)
		args = append(args, val)
	}
	if u := q.Get("user"); u != "" {
		add("(u.email LIKE", "%"+u+"%")
		where += ")"
	}
	if a := q.Get("action"); a != "" {
		add("a.action =", a)
	}
	if s := q.Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			add("a.ts >=", t)
		}
	}
	if u := q.Get("until"); u != "" {
		if t, err := time.Parse(time.RFC3339, u); err == nil {
			add("a.ts <=", t)
		}
	}

	// Total count — runs against the same WHERE without limit/offset.
	var total int
	countSQL := `
		SELECT COUNT(*)
		  FROM audit_log a
		  LEFT JOIN users u ON u.id = a.user_id
		 WHERE ` + where
	if err := h.DB.QueryRow(r.Context(), countSQL, args...).Scan(&total); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	args = append(args, limit, offset)
	sqlStr := `
		SELECT a.id, a.user_id::text, u.email, a.action,
		       a.bucket_name, a.object_key, a.size_bytes, a.status_code,
		       host(a.ip_address)::text, a.ts
		  FROM audit_log a
		  LEFT JOIN users u ON u.id = a.user_id
		 WHERE ` + where + `
		 ORDER BY a.ts DESC
		 LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))

	rows, err := h.DB.Query(r.Context(), sqlStr, args...)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	out := []AuditDTO{}
	for rows.Next() {
		var e AuditDTO
		if err := rows.Scan(&e.ID, &e.UserID, &e.UserEmail, &e.Action,
			&e.Bucket, &e.Key, &e.SizeBytes, &e.StatusCode,
			&e.IPAddress, &e.Timestamp); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "SCAN", err.Error())
			return
		}
		out = append(out, e)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"entries": out, "total": total, "limit": limit, "offset": offset,
	})
}

// ============================================================================
// SYSTEM STATS
// ============================================================================

type StatsDTO struct {
	UserCount       int           `json:"userCount"`
	BucketCount     int           `json:"bucketCount"`
	ObjectCount     int           `json:"objectCount"`
	TotalUsedBytes  int64         `json:"totalUsedBytes"`   // sum of users.used_bytes
	TotalQuotaBytes int64         `json:"totalQuotaBytes"`  // sum of users.quota_bytes
	TranscodeJobs   StatsJobsDTO  `json:"transcodeJobs"`
	RequestsLastHr  int           `json:"requestsLastHour"`
	TopActions      []ActionCount `json:"topActions"`
	Disk            *DiskInfoDTO  `json:"disk,omitempty"`     // physical disk facts
}

// DiskInfoDTO surfaces both the raw filesystem stats and the system's
// configured caps. Use both to render the "Storage" admin panel.
type DiskInfoDTO struct {
	Path             string `json:"path"`              // where /storage is mounted
	PhysicalTotal    uint64 `json:"physicalTotal"`     // total bytes on disk
	PhysicalUsed     uint64 `json:"physicalUsed"`      // used by ANYTHING (OS, db, our files...)
	PhysicalFree     uint64 `json:"physicalFree"`      // available to write
	ReservedBytes    int64  `json:"reservedBytes"`     // admin-configured headroom
	EffectiveTotal   int64  `json:"effectiveTotal"`    // what we'll allocate to users (cap)
	AllocatedToUsers int64  `json:"allocatedToUsers"`  // sum of users.quota_bytes
	ActuallyUsed     int64  `json:"actuallyUsed"`      // sum of users.used_bytes (db side)
	OvercommitAllowed bool  `json:"overcommitAllowed"`
	DiskFullPct       int   `json:"diskFullPct"`       // upload reject threshold
	Overcommitted     bool  `json:"overcommitted"`     // sum quotas > effective?
}

type StatsJobsDTO struct {
	Pending    int `json:"pending"`
	Processing int `json:"processing"`
	Done       int `json:"done"`
	Failed     int `json:"failed"`
}

type ActionCount struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
}

// GET /admin/stats
func (h *AdminHandler) Stats(w http.ResponseWriter, r *http.Request) {
	var s StatsDTO
	ctx := r.Context()

	// Single round-trip for the four scalar counts.
	err := h.DB.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM users WHERE is_active),
		  (SELECT COUNT(*) FROM buckets),
		  (SELECT COUNT(*) FROM objects WHERE NOT is_deleted),
		  (SELECT COALESCE(SUM(used_bytes), 0) FROM users),
		  (SELECT COALESCE(SUM(quota_bytes), 0) FROM users)`,
	).Scan(&s.UserCount, &s.BucketCount, &s.ObjectCount,
		&s.TotalUsedBytes, &s.TotalQuotaBytes)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Transcode job tallies
	jobRows, err := h.DB.Query(ctx,
		`SELECT status, COUNT(*) FROM transcode_jobs GROUP BY status`)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	for jobRows.Next() {
		var status string
		var n int
		if err := jobRows.Scan(&status, &n); err == nil {
			switch status {
			case "pending":    s.TranscodeJobs.Pending    = n
			case "processing": s.TranscodeJobs.Processing = n
			case "done":       s.TranscodeJobs.Done       = n
			case "failed":     s.TranscodeJobs.Failed     = n
			}
		}
	}
	jobRows.Close()

	// Requests in the last hour
	_ = h.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE ts > now() - INTERVAL '1 hour'`,
	).Scan(&s.RequestsLastHr)

	// Top 5 actions in the last hour
	topRows, err := h.DB.Query(ctx, `
		SELECT action, COUNT(*) c FROM audit_log
		 WHERE ts > now() - INTERVAL '1 hour'
		 GROUP BY action ORDER BY c DESC LIMIT 5`)
	if err == nil {
		for topRows.Next() {
			var a ActionCount
			if err := topRows.Scan(&a.Action, &a.Count); err == nil {
				s.TopActions = append(s.TopActions, a)
			}
		}
		topRows.Close()
	}

	// Disk facts — read filesystem, combine with config
	if d, err := storage.Stat(h.StorageRoot); err == nil {
		reserved := sysconfig.GetInt64(ctx, h.DB, sysconfig.KeyReservedBytes, 0)
		configuredTotal := sysconfig.GetInt64(ctx, h.DB, sysconfig.KeyTotalQuotaBytes, 0)
		overcommit := sysconfig.GetBool(ctx, h.DB, sysconfig.KeyOvercommitAllowed, false)
		thresholdPct := int(sysconfig.GetInt64(ctx, h.DB, sysconfig.KeyDiskFullThresholdPct, 95))

		effective := configuredTotal
		if effective == 0 {
			// Auto from physical disk minus reserved
			effective = int64(d.Total) - reserved
			if effective < 0 {
				effective = 0
			}
		}

		s.Disk = &DiskInfoDTO{
			Path:              d.Path,
			PhysicalTotal:     d.Total,
			PhysicalUsed:      d.Used,
			PhysicalFree:      d.Free,
			ReservedBytes:     reserved,
			EffectiveTotal:    effective,
			AllocatedToUsers:  s.TotalQuotaBytes,
			ActuallyUsed:      s.TotalUsedBytes,
			OvercommitAllowed: overcommit,
			DiskFullPct:       thresholdPct,
			Overcommitted:     s.TotalQuotaBytes > effective,
		}
	}

	httpx.WriteJSON(w, http.StatusOK, s)
}

// effectiveSystemTotal returns the bytes the system is willing to allocate to
// users — either the explicitly-configured cap, or auto-derived from the
// physical disk minus reserved bytes.
func effectiveSystemTotal(ctx context.Context, db *pgxpool.Pool, storageRoot string) int64 {
	configured := sysconfig.GetInt64(ctx, db, sysconfig.KeyTotalQuotaBytes, 0)
	if configured > 0 {
		return configured
	}
	d, err := storage.Stat(storageRoot)
	if err != nil {
		return 0
	}
	reserved := sysconfig.GetInt64(ctx, db, sysconfig.KeyReservedBytes, 0)
	effective := int64(d.Total) - reserved
	if effective < 0 {
		return 0
	}
	return effective
}

// ============================================================================
// CAPACITY POLICY — admin-tunable caps enforced everywhere a user is born
// or allocated more space.
//
// All the read-side state these need (user count, allocated sum, disk
// stats) is computed inline so callers don't need to thread it. Each
// query is cheap; we accept the small overhead per signup/approval over
// the complexity of caching invalidation.
// ============================================================================

// ActiveUserCount returns the number of users that count toward the
// max_users cap. Admin role still counts (it's still a person). Inactive
// users (is_active=false) are excluded — they can't sign in anyway.
func ActiveUserCount(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	var n int64
	err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE is_active = true`).Scan(&n)
	return n, err
}

// TotalAllocatedBytes returns SUM(users.quota_bytes) — what the admin
// has promised across the entire fleet.
func TotalAllocatedBytes(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	var n int64
	err := db.QueryRow(ctx,
		`SELECT COALESCE(SUM(quota_bytes), 0) FROM users WHERE is_active = true`).Scan(&n)
	return n, err
}

// CapacityState captures everything callers need to decide accept/reject.
// One struct + one constructor means the dashboard and the policy
// enforcement read EXACTLY the same numbers — no drift.
type CapacityState struct {
	UserCount       int64
	MaxUsers        int64 // 0 = unlimited
	AllocatedBytes  int64
	EffectiveTotal  int64
	MaxAllocPct     int64
	OvercommitOK    bool
}

// AllocationCap returns the bytes ceiling the SUM of user quotas may
// reach. Always-positive; 0 means no disk info (which we treat as
// "deny everything new" to be safe).
func (c CapacityState) AllocationCap() int64 {
	if c.EffectiveTotal <= 0 {
		return 0
	}
	pct := c.MaxAllocPct
	if pct <= 0 || pct > 100 {
		pct = 90
	}
	return c.EffectiveTotal * pct / 100
}

// CapacityHere reads the live capacity state. Cheap — three queries.
func CapacityHere(ctx context.Context, db *pgxpool.Pool, storageRoot string) (CapacityState, error) {
	uc, err := ActiveUserCount(ctx, db)
	if err != nil {
		return CapacityState{}, err
	}
	ab, err := TotalAllocatedBytes(ctx, db)
	if err != nil {
		return CapacityState{}, err
	}
	return CapacityState{
		UserCount:      uc,
		MaxUsers:       sysconfig.GetInt64(ctx, db, sysconfig.KeyMaxUsers, 100),
		AllocatedBytes: ab,
		EffectiveTotal: effectiveSystemTotal(ctx, db, storageRoot),
		MaxAllocPct:    sysconfig.GetInt64(ctx, db, sysconfig.KeyMaxAllocationPct, 90),
		OvercommitOK:   sysconfig.GetBool(ctx, db, sysconfig.KeyOvercommitAllowed, false),
	}, nil
}

// CanAdmitNewUser returns nil if the system can accept a 101st (or
// 1001st) user — checks both the user-count cap AND that the user's
// future quota wouldn't push the allocation past max_allocation_pct.
// `requestedQuota` may be 0 to skip the allocation check (e.g. when
// you only want to know "is there a seat").
func (c CapacityState) CanAdmitNewUser(requestedQuota int64) error {
	if c.MaxUsers > 0 && c.UserCount >= c.MaxUsers {
		return ErrCapacityFull
	}
	if requestedQuota > 0 && !c.OvercommitOK {
		if c.AllocatedBytes+requestedQuota > c.AllocationCap() {
			return ErrOverAllocation
		}
	}
	return nil
}

// CanGrantQuotaDelta returns nil if increasing existing-or-new quota by
// `additional` bytes wouldn't push allocation past max_allocation_pct.
// Used by admin user-edit and quota-request approvals.
func (c CapacityState) CanGrantQuotaDelta(additional int64) error {
	if additional <= 0 {
		return nil // shrinking quotas never fails
	}
	if c.OvercommitOK {
		return nil
	}
	if c.AllocatedBytes+additional > c.AllocationCap() {
		return ErrOverAllocation
	}
	return nil
}

// IsUserOverflow returns true when the given user is over the max_users
// cap. The "extra" set is the users with the NEWEST created_at — they
// were the last in, they're the first soft-blocked. Tiebreak by id so
// behaviour is deterministic.
//
// Returns false when the cap is 0 (unlimited) OR when user count is at
// or below the cap.
func (c CapacityState) IsUserOverflow(ctx context.Context, db *pgxpool.Pool, userID uuid.UUID) (bool, error) {
	if c.MaxUsers <= 0 || c.UserCount <= c.MaxUsers {
		return false, nil
	}
	// Rank by created_at ASC — the OLDEST max_users users get to stay.
	// Anyone with rank > max_users is overflow.
	var rank int64
	err := db.QueryRow(ctx, `
		WITH ranked AS (
			SELECT id, ROW_NUMBER() OVER (ORDER BY created_at, id) AS rn
			  FROM users WHERE is_active = true
		)
		SELECT rn FROM ranked WHERE id = $1`, userID).Scan(&rank)
	if err != nil {
		return false, err
	}
	return rank > c.MaxUsers, nil
}

// ErrCapacityFull → admin must raise max_users or deactivate someone first.
var ErrCapacityFull = errors.New("user cap reached — raise max_users or free a seat")

// ErrOverAllocation → granting this would push SUM(quotas) past
// max_allocation_pct of effective disk.
var ErrOverAllocation = errors.New("would exceed allocation cap — free space or toggle overcommit_allowed")

// ============================================================================
// SYSTEM CONFIG
// ============================================================================

// GET /admin/system-config
func (h *AdminHandler) GetSystemConfig(w http.ResponseWriter, r *http.Request) {
	entries, err := sysconfig.All(r.Context(), h.DB)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"config": entries})
}

// PATCH /admin/system-config/{key}
func (h *AdminHandler) UpdateSystemConfig(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")

	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}

	// Validate certain keys
	switch key {
	case sysconfig.KeyTotalQuotaBytes, sysconfig.KeyReservedBytes:
		n, err := strconv.ParseInt(req.Value, 10, 64)
		if err != nil || n < 0 {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_VALUE",
				"must be a non-negative integer (bytes)")
			return
		}
		// If lowering total_quota_bytes, ensure it doesn't drop below current allocations
		if key == sysconfig.KeyTotalQuotaBytes && n > 0 {
			var allocated int64
			_ = h.DB.QueryRow(r.Context(),
				`SELECT COALESCE(SUM(quota_bytes), 0) FROM users`,
			).Scan(&allocated)
			overcommit := sysconfig.GetBool(r.Context(), h.DB,
				sysconfig.KeyOvercommitAllowed, false)
			if !overcommit && n < allocated {
				httpx.WriteError(w, http.StatusConflict, "WOULD_OVERCOMMIT",
					fmt.Sprintf("cannot set total below currently allocated quotas (%d). "+
						"Lower user quotas first or enable overcommit_allowed.", allocated))
				return
			}
		}
	case sysconfig.KeyDiskFullThresholdPct:
		n, err := strconv.ParseInt(req.Value, 10, 64)
		if err != nil || n < 50 || n > 100 {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_VALUE",
				"must be 50-100")
			return
		}
	case sysconfig.KeyOvercommitAllowed,
		sysconfig.KeyCleanupDryRun,
		sysconfig.KeyOrphanTwoStrike:
		if req.Value != "true" && req.Value != "false" {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_VALUE",
				"must be 'true' or 'false'")
			return
		}
	case sysconfig.KeyOrphanMinAgeMinutes,
		sysconfig.KeyBloomRebuildHours,
		sysconfig.KeyCleanupIntervalSeconds,
		sysconfig.KeyMaxUsers:
		// Non-negative integers. 0 has meaning per-key (e.g. max_users=0
		// is unlimited, orphan_min_age_minutes=0 disables the gate),
		// so we don't enforce >0 here.
		n, err := strconv.ParseInt(req.Value, 10, 64)
		if err != nil || n < 0 {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_VALUE",
				"must be a non-negative integer")
			return
		}
		// Lowering max_users below current count is allowed — it
		// soft-blocks "extra" users at login. That's the documented
		// behaviour. So no extra guard here.
	case sysconfig.KeyMaxAllocationPct:
		n, err := strconv.ParseInt(req.Value, 10, 64)
		if err != nil || n < 1 || n > 100 {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_VALUE",
				"must be a percentage 1-100")
			return
		}
	case sysconfig.KeyDefaultMaxTranscodeDurationSeconds:
		// Seconds. 0 = unlimited. Upper bound 24h to catch fat-fingered values.
		n, err := strconv.ParseInt(req.Value, 10, 64)
		if err != nil || n < 0 || n > 86400 {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_VALUE",
				"must be a non-negative integer ≤ 86400 (24h). 0 = unlimited")
			return
		}
	case sysconfig.KeyDefaultMaxTranscodeHeight:
		// Pixels. 0 = unlimited. Upper bound 8K-ish so admins can't accidentally
		// set 100000 and think it means something.
		n, err := strconv.ParseInt(req.Value, 10, 64)
		if err != nil || n < 0 || n > 4320 {
			httpx.WriteError(w, http.StatusBadRequest, "BAD_VALUE",
				"must be a non-negative integer ≤ 4320 (8K). 0 = unlimited")
			return
		}
	default:
		httpx.WriteError(w, http.StatusBadRequest, "UNKNOWN_KEY",
			"unknown config key: "+key)
		return
	}

	if err := sysconfig.Set(r.Context(), h.DB, key, req.Value); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /admin/transcode-jobs?status=&limit=&offset=
//
// Browse the transcode job queue — pending / processing / done / failed /
// skipped. Used by the Logs page in the dashboard so admins can spot
// stuck pipelines without dropping to psql.
func (h *AdminHandler) TranscodeJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	offset := 0
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n >= 0 {
		offset = n
	}

	where := "TRUE"
	args := []any{}
	if s := q.Get("status"); s != "" {
		args = append(args, s)
		where = fmt.Sprintf("status = $%d", len(args))
	}

	// Total count for paging UI.
	var total int
	countSQL := fmt.Sprintf("SELECT COUNT(*) FROM transcode_jobs WHERE %s", where)
	if err := h.DB.QueryRow(r.Context(), countSQL, args...).Scan(&total); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	args = append(args, limit, offset)
	sqlStr := fmt.Sprintf(`
		SELECT tj.id, tj.object_id, tj.file_type, tj.status, tj.attempts,
		       tj.priority, tj.progress_pct,
		       COALESCE(tj.error, ''), tj.created_at, tj.started_at, tj.done_at,
		       COALESCE(o.key, '<deleted>'), COALESCE(b.name, '<deleted>')
		  FROM transcode_jobs tj
		  LEFT JOIN objects o ON o.id = tj.object_id
		  LEFT JOIN buckets b ON b.id = o.bucket_id
		 WHERE %s
		 ORDER BY tj.created_at DESC
		 LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args))

	rows, err := h.DB.Query(r.Context(), sqlStr, args...)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	type job struct {
		ID          string     `json:"id"`
		ObjectID    string     `json:"objectId"`
		FileType    string     `json:"fileType"`
		Status      string     `json:"status"`
		Attempts    int        `json:"attempts"`
		Priority    int        `json:"priority"`
		ProgressPct int        `json:"progressPct"`
		Error       string     `json:"error"`
		CreatedAt   time.Time  `json:"createdAt"`
		StartedAt   *time.Time `json:"startedAt,omitempty"`
		DoneAt      *time.Time `json:"doneAt,omitempty"`
		Key         string     `json:"key"`
		Bucket      string     `json:"bucket"`
	}
	out := []job{}
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.ID, &j.ObjectID, &j.FileType, &j.Status,
			&j.Attempts, &j.Priority, &j.ProgressPct, &j.Error,
			&j.CreatedAt, &j.StartedAt, &j.DoneAt,
			&j.Key, &j.Bucket); err != nil {
			continue
		}
		out = append(out, j)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"jobs": out, "total": total, "limit": limit, "offset": offset,
	})
}

// GET /admin/quota-events — Server-Sent Events stream of every QuotaReserve
// call (charge / refund / rejected). One JSON object per event:
//
//   data: {"ts":"...","userId":"...","delta":5242880,"newBytes":...,"caller":"multipart.go:185"}
//
// Tail with: curl -N -H "Authorization: Bearer $JWT" http://host/api/admin/quota-events
//
// Admin-only; the route is registered under the admin block so the auth
// middleware enforces role=admin.
func (h *AdminHandler) QuotaEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx proxy buffering

	ch, cancel := middleware.SubscribeQuotaEvents()
	defer cancel()

	// Initial comment so clients flush their connect callback immediately.
	_, _ = w.Write([]byte(": connected — every QuotaReserve will appear here\n\n"))
	flusher.Flush()

	// Heartbeat every 15s so proxies / load balancers don't time out the
	// connection during quiet periods.
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			b, _ := json.Marshal(e)
			if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// GET /admin/quota-events/history?limit=&offset=&user= — paginated historical
// view of the quota_events table. Companion to the live SSE endpoint above:
// SSE shows new events; this lets admins page back through what's already
// happened.
func (h *AdminHandler) QuotaEventsHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	offset := 0
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n >= 0 {
		offset = n
	}

	where := "TRUE"
	args := []any{}
	if u := q.Get("user"); u != "" {
		// Looking up by email — JOIN against users so the UI doesn't need to
		// resolve UUIDs first.
		args = append(args, "%"+u+"%")
		where = "u.email ILIKE $" + strconv.Itoa(len(args))
	}

	// Total count for the page footer.
	var total int
	countSQL := `
		SELECT COUNT(*)
		  FROM quota_events qe
		  LEFT JOIN users u ON u.id = qe.user_id
		 WHERE ` + where
	if err := h.DB.QueryRow(r.Context(), countSQL, args...).Scan(&total); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	args = append(args, limit, offset)
	rowsSQL := `
		SELECT qe.ts, qe.user_id, qe.delta, qe.new_bytes, qe.caller, qe.rejected,
		       COALESCE(u.email, '')
		  FROM quota_events qe
		  LEFT JOIN users u ON u.id = qe.user_id
		 WHERE ` + where + `
		 ORDER BY qe.ts DESC
		 LIMIT $` + strconv.Itoa(len(args)-1) + ` OFFSET $` + strconv.Itoa(len(args))

	rows, err := h.DB.Query(r.Context(), rowsSQL, args...)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()

	type evt struct {
		TS        time.Time `json:"ts"`
		UserID    string    `json:"userId"`
		UserEmail string    `json:"userEmail"`
		Delta     int64     `json:"delta"`
		NewBytes  int64     `json:"newBytes"`
		Caller    string    `json:"caller"`
		Rejected  bool      `json:"rejected"`
	}
	out := []evt{}
	for rows.Next() {
		var e evt
		if err := rows.Scan(&e.TS, &e.UserID, &e.Delta, &e.NewBytes, &e.Caller, &e.Rejected, &e.UserEmail); err != nil {
			continue
		}
		out = append(out, e)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"events": out, "total": total, "limit": limit, "offset": offset,
	})
}

// ============================================================================
// TRANSCODE JOB CONTROL
//
// Admins need a way to intervene on stuck/failed/running transcode jobs
// without dropping to psql or docker exec. The three primitives below
// cover the realistic operator workflow:
//
//   DELETE /admin/transcode-jobs/{id}      → kill one job NOW
//   POST   /admin/transcode-jobs/{id}/retry → reset attempts, mark pending
//   PATCH  /admin/transcode-jobs/{id}      → re-prioritize (move up/down in queue)
//
// Plus a higher-level operation to retry an entire object's pipeline:
//
//   POST   /admin/objects/{id}/retry-transcode → wipe + re-queue every rung
//
// All four go through AdminOnly so a regular user can't pretend to be the
// owner of someone else's object.
// ============================================================================

// DELETE /admin/transcode-jobs/{id} — cancel one job.
// Publishes a cancel to any worker holding the ffmpeg PID, then deletes
// the row. The worker also notices the missing row on the next progress
// check and exits ffmpeg as a fallback.
func (h *AdminHandler) CancelTranscodeJob(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	jobID, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid uuid")
		return
	}
	// Look up the row first so we can tell the worker to stop AND so we
	// can return a useful "what did we just kill" body to the admin.
	var objectID uuid.UUID
	var fileType, status string
	err = h.DB.QueryRow(r.Context(),
		`SELECT object_id, file_type, status FROM transcode_jobs WHERE id = $1`, jobID,
	).Scan(&objectID, &fileType, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	cache.PublishCancelJob(r.Context(), h.RDB, jobID, "admin cancel")
	if _, err := h.DB.Exec(r.Context(),
		`DELETE FROM transcode_jobs WHERE id = $1`, jobID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"cancelled": true, "jobId": jobID, "objectId": objectID,
		"fileType": fileType, "wasStatus": status,
	})
}

// POST /admin/transcode-jobs/{id}/retry — reset to pending, attempts=0.
// Doesn't trigger anything actively; the next worker poll picks it up.
func (h *AdminHandler) RetryTranscodeJob(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	jobID, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid uuid")
		return
	}
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE transcode_jobs
		   SET status = 'pending',
		       attempts = 0,
		       progress_pct = 0,
		       error = NULL,
		       started_at = NULL,
		       done_at = NULL,
		       updated_at = now()
		 WHERE id = $1`, jobID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusNotFound, "NOT_FOUND", "job not found")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"retried": true, "jobId": jobID})
}

// PATCH /admin/transcode-jobs/{id} — change priority. Body: {"priority": int}.
// Lower number = higher priority (workers ORDER BY priority ASC). Only
// pending/waiting jobs get re-prioritized — touching processing/done/failed
// is meaningless.
func (h *AdminHandler) UpdateTranscodeJob(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	jobID, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid uuid")
		return
	}
	var req struct {
		Priority *int `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	if req.Priority == nil {
		httpx.WriteError(w, http.StatusBadRequest, "NO_FIELDS", "priority is required")
		return
	}
	tag, err := h.DB.Exec(r.Context(),
		`UPDATE transcode_jobs SET priority = $2, updated_at = now()
		  WHERE id = $1 AND status IN ('pending', 'waiting')`,
		jobID, *req.Priority)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusConflict, "NOT_QUEUED",
			"only pending/waiting jobs can be re-prioritized")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"updated": true, "jobId": jobID, "priority": *req.Priority,
	})
}

// POST /admin/objects/{id}/retry-transcode — rebuild a whole pipeline.
// Mirrors the user-facing POST /:bucket/*?transcode, but works on any
// object regardless of owner. Used when a failed pipeline has eaten its
// retries (3 attempts each) and the admin wants a fresh start.
//
// Wired to insertTranscodeJob (the same helper the user endpoint calls)
// so transcode limits, file-type detection, and quota checks all behave
// identically.
func (h *AdminHandler) RetryObjectTranscode(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	objectID, err := uuid.Parse(idStr)
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid uuid")
		return
	}
	var bucketID uuid.UUID
	var key, contentType string
	err = h.DB.QueryRow(r.Context(),
		`SELECT bucket_id, key, content_type
		   FROM objects WHERE id = $1 AND NOT is_deleted`,
		objectID,
	).Scan(&bucketID, &key, &contentType)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NOT_FOUND", "object not found or deleted")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	ft := detectFileType(contentType, key)
	if ft == "" {
		httpx.WriteError(w, http.StatusBadRequest, "NOT_MEDIA",
			"this file isn't a recognized video/audio/image type")
		return
	}
	// Same atomic-cancel-then-re-insert dance as the user endpoint.
	_, _ = h.DB.Exec(r.Context(),
		`DELETE FROM transcode_jobs WHERE object_id = $1`, objectID)
	cache.PublishCancelObject(r.Context(), h.RDB, objectID, "admin retry")
	insertTranscodeJob(r.Context(), h.DB, h.FS, objectID, bucketID, key, ft)
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{
		"objectId": objectID, "key": key, "fileType": ft, "status": "pending",
	})
}
