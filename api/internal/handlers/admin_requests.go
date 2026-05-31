// Admin endpoints for the self-serve onboarding queue.
//
//   GET   /admin/requests                          → both queues
//   POST  /admin/requests/account/{id}/approve     → create user + email OTP
//   POST  /admin/requests/account/{id}/deny        → mark denied + email
//   POST  /admin/requests/quota/{id}/approve       → bump quota_bytes + email
//   POST  /admin/requests/quota/{id}/deny          → mark denied + email
//   GET   /admin/email-outbox                      → dev-mode email list
//   GET   /admin/email-outbox/{file}               → one .eml body
//
// All require role=admin (the parent route applies that middleware).

package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/email"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

// AdminRequestsHandler is the admin surface for both queues.
type AdminRequestsHandler struct {
	DB                *pgxpool.Pool
	Mailer            *email.Mailer
	OutboxDir         string
	StorageRoot       string // needed for the allocation-cap check
	AdminEmail        string
	SupportEmail      string // address put in user-facing email bodies
	LoginURL          string
	DefaultQuotaBytes int64
}

// supportEmail returns the address surfaced in user-facing email
// templates' "Need help?" lines. Falls back to AdminEmail when blank.
func (h *AdminRequestsHandler) supportEmail() string {
	if strings.TrimSpace(h.SupportEmail) != "" {
		return h.SupportEmail
	}
	return h.AdminEmail
}

// ---------- GET /admin/requests --------------------------------------------

type accountReqDTO struct {
	ID                uuid.UUID `json:"id"`
	Email             string    `json:"email"`
	Name              string    `json:"name"`
	Reason            string    `json:"reason"`
	Status            string    `json:"status"`
	GrantedQuotaBytes *int64    `json:"grantedQuotaBytes,omitempty"`
	AdminNote         string    `json:"adminNote"`
	RequestedAt       time.Time `json:"requestedAt"`
	DecidedAt         *time.Time `json:"decidedAt,omitempty"`
}

type quotaReqDTO struct {
	ID             uuid.UUID `json:"id"`
	UserID         uuid.UUID `json:"userId"`
	UserEmail      string    `json:"userEmail"`
	UserName       string    `json:"userName"`
	CurrentBytes   int64     `json:"currentBytes"`
	UsedBytes      int64     `json:"usedBytes"`
	RequestedBytes int64     `json:"requestedBytes"`
	Reason         string    `json:"reason"`
	Status         string    `json:"status"`
	GrantedBytes   *int64    `json:"grantedBytes,omitempty"`
	AdminNote      string    `json:"adminNote"`
	RequestedAt    time.Time `json:"requestedAt"`
	DecidedAt      *time.Time `json:"decidedAt,omitempty"`
}

func (h *AdminRequestsHandler) List(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}

	accountRows, _ := h.DB.Query(r.Context(), `
		SELECT id, email, name, reason, status,
		       granted_quota_bytes, admin_note,
		       requested_at, decided_at
		  FROM account_requests
		 WHERE ($1 = 'all' OR status = $1)
		 ORDER BY requested_at DESC LIMIT 200`, status)
	defer accountRows.Close()
	accounts := []accountReqDTO{}
	for accountRows.Next() {
		var a accountReqDTO
		_ = accountRows.Scan(&a.ID, &a.Email, &a.Name, &a.Reason, &a.Status,
			&a.GrantedQuotaBytes, &a.AdminNote, &a.RequestedAt, &a.DecidedAt)
		accounts = append(accounts, a)
	}

	quotaRows, _ := h.DB.Query(r.Context(), `
		SELECT q.id, q.user_id, u.email, u.name,
		       u.quota_bytes, u.used_bytes,
		       q.requested_bytes, q.reason, q.status,
		       q.granted_bytes, q.admin_note,
		       q.requested_at, q.decided_at
		  FROM quota_requests q
		  JOIN users u ON u.id = q.user_id
		 WHERE ($1 = 'all' OR q.status = $1)
		 ORDER BY q.requested_at DESC LIMIT 200`, status)
	defer quotaRows.Close()
	quotas := []quotaReqDTO{}
	for quotaRows.Next() {
		var q quotaReqDTO
		_ = quotaRows.Scan(&q.ID, &q.UserID, &q.UserEmail, &q.UserName,
			&q.CurrentBytes, &q.UsedBytes,
			&q.RequestedBytes, &q.Reason, &q.Status,
			&q.GrantedBytes, &q.AdminNote,
			&q.RequestedAt, &q.DecidedAt)
		quotas = append(quotas, q)
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"accounts": accounts,
		"quotas":   quotas,
	})
}

// ---------- POST /admin/requests/account/{id}/approve ----------------------

type approveAccountReq struct {
	QuotaBytes int64  `json:"quotaBytes"` // override; 0 = use default
	Note       string `json:"note"`
}

func (h *AdminRequestsHandler) ApproveAccount(w http.ResponseWriter, r *http.Request) {
	admin := middleware.MustUser(r.Context())
	reqID, perr := uuid.Parse(chi.URLParam(r, "id"))
	if perr != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid id")
		return
	}
	var body approveAccountReq
	_ = json.NewDecoder(r.Body).Decode(&body)
	quota := body.QuotaBytes
	if quota <= 0 {
		quota = h.DefaultQuotaBytes
	}
	if quota <= 0 {
		quota = 100 * 1024 * 1024
	}

	// Capacity check up-front — bail before we open a transaction.
	// Both the user-count cap AND the per-quota allocation cap apply.
	// Admin can fix this by raising max_users / max_allocation_pct or
	// granting a smaller quota.
	if cap, capErr := CapacityHere(r.Context(), h.DB, h.StorageRoot); capErr == nil {
		if err := cap.CanAdmitNewUser(quota); err != nil {
			code := "CAPACITY_FULL"
			if errors.Is(err, ErrOverAllocation) {
				code = "OVER_ALLOCATION"
			}
			httpx.WriteErrorDetails(w, http.StatusConflict, code, err.Error(),
				map[string]any{
					"userCount":      cap.UserCount,
					"maxUsers":       cap.MaxUsers,
					"allocatedBytes": cap.AllocatedBytes,
					"requestedBytes": quota,
					"allocationCap":  cap.AllocationCap(),
				})
			return
		}
	}

	// Pull + lock the request row in one statement; fail if not pending.
	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "TX", err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	var emailAddr, name string
	err = tx.QueryRow(r.Context(), `
		SELECT email, name FROM account_requests
		 WHERE id = $1 AND status = 'pending'
		 FOR UPDATE`, reqID).Scan(&emailAddr, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NOT_PENDING",
			"request not found or already decided")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Don't re-create if a row already exists for this email.
	var userID uuid.UUID
	err = tx.QueryRow(r.Context(), `
		SELECT id FROM users WHERE lower(email) = lower($1)`, emailAddr).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(r.Context(), `
			INSERT INTO users (email, name, role, quota_bytes, is_active)
			VALUES ($1, $2, 'user', $3, true)
			RETURNING id`, emailAddr, name, quota).Scan(&userID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
			return
		}
	}

	_, err = tx.Exec(r.Context(), `
		UPDATE account_requests
		   SET status='approved',
		       granted_quota_bytes=$1,
		       admin_note=$2,
		       decided_at=now(),
		       decided_by=$3,
		       created_user_id=$4
		 WHERE id=$5`,
		quota, body.Note, admin.ID, userID, reqID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Issue + send a verify-email OTP so the user can set their password.
	code, codeHash := genOTP("account_verification")
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO otp_codes (code_hash, purpose, email, user_id, expires_at)
		VALUES ($1, 'account_verification', $2, $3, now() + interval '24 hours')`,
		codeHash, emailAddr, userID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "COMMIT", err.Error())
		return
	}

	if h.Mailer != nil {
		_, _ = h.Mailer.SendTemplate(emailAddr,
			"Your PersonalS3 access was approved",
			"account_approved",
			map[string]any{
				"Brand":        "PersonalS3",
				"Name":         name,
				"Email":        emailAddr,
				"QuotaHuman":   email.FormatBytes(quota),
				"LoginURL":     h.LoginURL,
				"SupportEmail": h.supportEmail(),
			})
		_, _ = h.Mailer.SendTemplate(emailAddr,
			"Verify your PersonalS3 email",
			"verify_email",
			map[string]any{
				"Brand":        "PersonalS3",
				"Name":         name,
				"Code":         code,
				"TTLMinutes":   24 * 60,
				"SupportEmail": h.supportEmail(),
			})
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"approved":   true,
		"userId":     userID,
		"quotaBytes": quota,
		"otpSent":    h.Mailer != nil,
	})
}

// ---------- POST /admin/requests/account/{id}/deny -------------------------

type denyReq struct {
	Note string `json:"note"`
}

func (h *AdminRequestsHandler) DenyAccount(w http.ResponseWriter, r *http.Request) {
	admin := middleware.MustUser(r.Context())
	reqID, perr := uuid.Parse(chi.URLParam(r, "id"))
	if perr != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid id")
		return
	}
	var body denyReq
	_ = json.NewDecoder(r.Body).Decode(&body)

	var emailAddr string
	err := h.DB.QueryRow(r.Context(), `
		UPDATE account_requests
		   SET status='denied', admin_note=$1, decided_at=now(), decided_by=$2
		 WHERE id=$3 AND status='pending'
		 RETURNING email`,
		body.Note, admin.ID, reqID,
	).Scan(&emailAddr)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NOT_PENDING", "no such pending request")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if h.Mailer != nil {
		_, _ = h.Mailer.SendTemplate(emailAddr,
			"Your PersonalS3 access request",
			"account_denied",
			map[string]any{
				"Brand":        "PersonalS3",
				"Note":         body.Note,
				"AdminEmail":   h.AdminEmail,
				"SupportEmail": h.supportEmail(),
			})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"denied": true})
}

// ---------- POST /admin/requests/quota/{id}/approve ------------------------

type approveQuotaReq struct {
	GrantedBytes int64  `json:"grantedBytes"` // 0 = grant the requested amount
	Note         string `json:"note"`
}

func (h *AdminRequestsHandler) ApproveQuota(w http.ResponseWriter, r *http.Request) {
	admin := middleware.MustUser(r.Context())
	reqID, perr := uuid.Parse(chi.URLParam(r, "id"))
	if perr != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid id")
		return
	}
	var body approveQuotaReq
	_ = json.NewDecoder(r.Body).Decode(&body)

	tx, err := h.DB.Begin(r.Context())
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "TX", err.Error())
		return
	}
	defer tx.Rollback(r.Context())

	var userID uuid.UUID
	var requestedBytes int64
	var userEmail, userName string
	var currentQuota int64
	err = tx.QueryRow(r.Context(), `
		SELECT q.user_id, q.requested_bytes, u.email, u.name, u.quota_bytes
		  FROM quota_requests q JOIN users u ON u.id = q.user_id
		 WHERE q.id = $1 AND q.status = 'pending'
		 FOR UPDATE OF q`, reqID,
	).Scan(&userID, &requestedBytes, &userEmail, &userName, &currentQuota)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NOT_PENDING", "no such pending request")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	granted := body.GrantedBytes
	if granted <= 0 {
		granted = requestedBytes
	}

	// Allocation cap — granting `granted` more bytes must not push the
	// system past max_allocation_pct of effective disk.
	if cap, capErr := CapacityHere(r.Context(), h.DB, h.StorageRoot); capErr == nil {
		if err := cap.CanGrantQuotaDelta(granted); err != nil {
			httpx.WriteErrorDetails(w, http.StatusConflict, "OVER_ALLOCATION",
				err.Error(),
				map[string]any{
					"grantedBytes":      granted,
					"allocatedBytes":    cap.AllocatedBytes,
					"allocationCap":     cap.AllocationCap(),
					"maxAllocationPct":  cap.MaxAllocPct,
				})
			return
		}
	}
	newQuota := currentQuota + granted
	if _, err := tx.Exec(r.Context(),
		`UPDATE users SET quota_bytes = $1, updated_at = now() WHERE id = $2`,
		newQuota, userID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if _, err := tx.Exec(r.Context(), `
		UPDATE quota_requests
		   SET status='approved', granted_bytes=$1, admin_note=$2,
		       decided_at=now(), decided_by=$3
		 WHERE id=$4`,
		granted, body.Note, admin.ID, reqID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "COMMIT", err.Error())
		return
	}

	if h.Mailer != nil {
		_, _ = h.Mailer.SendTemplate(userEmail,
			"More PersonalS3 space has been granted",
			"quota_approved",
			map[string]any{
				"Brand":         "PersonalS3",
				"Name":          userName,
				"NewQuotaHuman": email.FormatBytes(newQuota),
				"GrantedHuman":  email.FormatBytes(granted),
				"Note":          body.Note,
				"SupportEmail":  h.supportEmail(),
			})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"approved":      true,
		"newQuotaBytes": newQuota,
		"grantedBytes":  granted,
	})
}

func (h *AdminRequestsHandler) DenyQuota(w http.ResponseWriter, r *http.Request) {
	admin := middleware.MustUser(r.Context())
	reqID, perr := uuid.Parse(chi.URLParam(r, "id"))
	if perr != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid id")
		return
	}
	var body denyReq
	_ = json.NewDecoder(r.Body).Decode(&body)

	var userEmail, userName string
	err := h.DB.QueryRow(r.Context(), `
		WITH d AS (
			UPDATE quota_requests
			   SET status='denied', admin_note=$1, decided_at=now(), decided_by=$2
			 WHERE id=$3 AND status='pending'
			 RETURNING user_id
		)
		SELECT u.email, u.name FROM users u
		  JOIN d ON u.id = d.user_id`,
		body.Note, admin.ID, reqID).Scan(&userEmail, &userName)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "NOT_PENDING", "no such pending request")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if h.Mailer != nil {
		_, _ = h.Mailer.SendTemplate(userEmail,
			"Your PersonalS3 quota request",
			"quota_denied",
			map[string]any{
				"Brand":        "PersonalS3",
				"Name":         userName,
				"Note":         body.Note,
				"SupportEmail": h.supportEmail(),
			})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"denied": true})
}

// ---------- GET /admin/email-outbox ---------------------------------------

type outboxEntry struct {
	File      string    `json:"file"`
	Modified  time.Time `json:"modified"`
	SizeBytes int64     `json:"sizeBytes"`
}

// OutboxList enumerates the dev-mode outbox. Returns empty when SMTP is
// live (the directory is unused but might exist from earlier dev runs).
func (h *AdminRequestsHandler) OutboxList(w http.ResponseWriter, r *http.Request) {
	entries := []outboxEntry{}
	if h.OutboxDir != "" {
		_ = filepath.Walk(h.OutboxDir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".eml") {
				return nil
			}
			rel, _ := filepath.Rel(h.OutboxDir, path)
			entries = append(entries, outboxEntry{
				File: rel, Modified: info.ModTime(), SizeBytes: info.Size(),
			})
			return nil
		})
	}
	// Newest first.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Modified.After(entries[j].Modified)
	})
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"entries": entries,
		"dir":     h.OutboxDir,
	})
}

// OutboxGet streams one .eml file as plain text.
func (h *AdminRequestsHandler) OutboxGet(w http.ResponseWriter, r *http.Request) {
	file := chi.URLParam(r, "*")
	if strings.Contains(file, "..") || strings.HasPrefix(file, "/") {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_FILE", "invalid filename")
		return
	}
	full := filepath.Join(h.OutboxDir, file)
	b, err := os.ReadFile(full)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_FILE", "outbox entry not found")
		return
	}
	w.Header().Set("Content-Type", "message/rfc822")
	_, _ = w.Write(b)
}
