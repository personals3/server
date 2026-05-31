// Self-serve onboarding handlers: account requests, email verification,
// password reset, quota requests.
//
// Design notes:
//
//   - Every public endpoint that takes an email is rate-limited at the
//     middleware layer and ALSO returns a fixed-shape 200 even when the
//     email doesn't exist (so we don't leak the user list).
//
//   - OTPs are 6 digits, single-use, 10-minute TTL. They're stored as
//     sha256("<purpose>|<code>") so a DB compromise doesn't yield
//     reusable codes.
//
//   - Account creation only happens via admin approval — never directly
//     from a public endpoint. Even the OTP "verify your email" step just
//     sets a password on an already-created (approved) row.

package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/auth"
	"github.com/personals3/api/internal/email"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

const otpTTL = 10 * 60 // seconds

// supportEmail returns the address to surface in user-facing email
// bodies. Prefers SupportEmail (which is usually SMTP_REPLY_TO), falls
// back to AdminEmail so even un-configured deployments have something
// human-readable in the "Need help?" line.
func (h *AuthHandler) supportEmail() string {
	if strings.TrimSpace(h.SupportEmail) != "" {
		return h.SupportEmail
	}
	return h.AdminEmail
}

// ---------- POST /auth/request-account -------------------------------------

type requestAccountReq struct {
	Email  string `json:"email"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// RequestAccount is the public form. We INSERT a pending row, dedupe by
// lowercased email (the unique-index does the heavy lifting), notify the
// admin, and ALWAYS return the same shape regardless of whether we found
// a duplicate or a wholly new request — to keep the endpoint
// non-enumerable.
func (h *AuthHandler) RequestAccount(w http.ResponseWriter, r *http.Request) {
	var req requestAccountReq
	if err := jsonBodyOpt(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	email := normalizeEmail(req.Email)
	if email == "" || !looksLikeEmail(email) {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_EMAIL",
			"a valid email is required")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}

	// Don't even create the request if the email already has an active user.
	var existingUserID uuid.UUID
	_ = h.DB.QueryRow(r.Context(),
		`SELECT id FROM users WHERE lower(email) = lower($1) AND is_active = true`,
		email,
	).Scan(&existingUserID)
	if existingUserID != uuid.Nil {
		// Same response shape — we don't tell strangers "this email exists".
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"received": true,
			"message":  "If your address is eligible, we'll email you when an administrator decides.",
		})
		return
	}

	// Capacity check — if the system is at the max_users cap, write the
	// row as already-denied. The admin still gets notified so they know
	// demand exists; the requester gets the same fixed-shape response so
	// the endpoint stays non-enumerable. They can raise the cap and
	// re-approve from the dashboard.
	cap, _ := CapacityHere(r.Context(), h.DB, h.FS.Root())
	atCapacity := cap.CanAdmitNewUser(0) != nil

	status := "pending"
	adminNote := ""
	if atCapacity {
		status = "denied"
		adminNote = "Auto-denied: instance was at max_users cap (" +
			strconv.FormatInt(cap.MaxUsers, 10) + ") when requested. Raise the cap and re-approve from the requests page."
	}

	tag, err := h.DB.Exec(r.Context(), `
		INSERT INTO account_requests (email, name, reason, status, admin_note,
		                              decided_at, decided_by)
		VALUES ($1, $2, $3, $4, $5,
		        CASE WHEN $4 = 'denied' THEN now() ELSE NULL END,
		        NULL)
		ON CONFLICT DO NOTHING`,
		email, name, strings.TrimSpace(req.Reason), status, adminNote)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	// Notify the admin only if the row is freshly inserted (not a dupe).
	if tag.RowsAffected() > 0 && h.Mailer != nil && h.AdminEmail != "" {
		subject := "PersonalS3 access request: " + email
		if atCapacity {
			subject = "[AUTO-DENIED, capacity full] " + subject
		}
		_, _ = h.Mailer.SendTemplate(h.AdminEmail,
			subject,
			"admin_new_account_request",
			map[string]any{
				"Brand":    "PersonalS3",
				"Email":    email,
				"Name":     name,
				"Reason":   firstNonEmpty(req.Reason, "(no reason given)") + capacityFootnote(atCapacity, cap),
				"AdminURL": h.AdminURL + "/dashboard/admin/requests",
			})
	}

	// Notify the requester they're soft-denied so they know to wait/contact.
	// We send the standard "received" copy to both pending AND auto-denied
	// because the truthful "we're full" hint would leak capacity info.
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"received": true,
		"message":  "If your address is eligible, we'll email you when an administrator decides.",
	})
}

func capacityFootnote(atCapacity bool, c CapacityState) string {
	if !atCapacity {
		return ""
	}
	return "\n\n[Auto-denied — instance was at max_users=" +
		strconv.FormatInt(c.MaxUsers, 10) +
		" when this request arrived. Raise the cap in System → Capacity and re-approve from the requests page if you want to admit them.]"
}

// ---------- POST /auth/forgot-password -------------------------------------

type forgotReq struct {
	Email string `json:"email"`
}

// Forgot generates an OTP, mails it to the user, and returns the same
// "if your address is on file we sent a code" message no matter what.
// The OTP is hashed before storage so a DB compromise can't reuse it.
func (h *AuthHandler) Forgot(w http.ResponseWriter, r *http.Request) {
	var req forgotReq
	if err := jsonBodyOpt(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	email := normalizeEmail(req.Email)
	if email == "" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_EMAIL", "email required")
		return
	}

	var userID uuid.UUID
	var name string
	err := h.DB.QueryRow(r.Context(),
		`SELECT id, name FROM users WHERE lower(email) = lower($1) AND is_active = true`,
		email,
	).Scan(&userID, &name)
	if err == nil {
		// User found — issue OTP + send email. We swallow errors here so
		// timing / outcome doesn't leak existence.
		code, codeHash := genOTP("password_reset")
		_, _ = h.DB.Exec(r.Context(), `
			INSERT INTO otp_codes (code_hash, purpose, email, user_id, expires_at)
			VALUES ($1, 'password_reset', $2, $3, now() + interval '10 minutes')`,
			codeHash, email, userID)
		if h.Mailer != nil {
			_, _ = h.Mailer.SendTemplate(email,
				"Reset your PersonalS3 password",
				"password_reset",
				map[string]any{
					"Brand":        "PersonalS3",
					"Email":        email,
					"Code":         code,
					"TTLMinutes":   otpTTL / 60,
					"SupportEmail": h.supportEmail(),
				})
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"sent":    true,
		"message": "If your address is on file, we sent a one-time code that expires in 10 minutes.",
	})
}

// ---------- POST /auth/reset-password --------------------------------------

type resetReq struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

// Reset consumes a password_reset OTP and sets a new password.
func (h *AuthHandler) Reset(w http.ResponseWriter, r *http.Request) {
	var req resetReq
	if err := jsonBodyOpt(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	email := normalizeEmail(req.Email)
	code := strings.TrimSpace(req.Code)
	if email == "" || code == "" || len(req.Password) < 8 {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST",
			"email, code, and password (>=8 chars) are required")
		return
	}

	codeHash := hashOTP("password_reset", code)
	var userID uuid.UUID
	err := h.DB.QueryRow(r.Context(), `
		UPDATE otp_codes
		   SET consumed_at = now()
		 WHERE code_hash = $1 AND purpose = 'password_reset'
		   AND email = $2
		   AND consumed_at IS NULL
		   AND expires_at > now()
		 RETURNING user_id`, codeHash, email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_CODE",
			"that code is invalid, expired, or already used")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "HASH", err.Error())
		return
	}
	if _, err := h.DB.Exec(r.Context(),
		`UPDATE users SET password_hash=$1, updated_at=now() WHERE id=$2`,
		hash, userID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	// Security: a password reset invalidates ALL trusted devices. If
	// someone else used the reset code, they don't also inherit the
	// legit owner's pre-trusted browsers.
	_, _ = h.DB.Exec(r.Context(),
		`DELETE FROM trusted_devices WHERE user_id = $1`, userID)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"reset":   true,
		"message": "Password updated. You can sign in now. Any previously-trusted devices have been logged out.",
	})
}

// ---------- POST /auth/verify-email ----------------------------------------
//
// Account creation flow: admin approves a request → backend creates the
// user row + issues a verify_email OTP → user receives the OTP and POSTs
// here with email+code+password to set their first password.

type emailVerifyReq struct {
	Email    string `json:"email"`
	Code     string `json:"code"`
	Password string `json:"password"`
}

func (h *AuthHandler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req emailVerifyReq
	if err := jsonBodyOpt(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	email := normalizeEmail(req.Email)
	code := strings.TrimSpace(req.Code)
	if email == "" || code == "" || len(req.Password) < 8 {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST",
			"email, code, and password (>=8 chars) are required")
		return
	}

	codeHash := hashOTP("account_verification", code)
	var userID uuid.UUID
	err := h.DB.QueryRow(r.Context(), `
		UPDATE otp_codes
		   SET consumed_at = now()
		 WHERE code_hash = $1 AND purpose = 'account_verification'
		   AND email = $2
		   AND consumed_at IS NULL
		   AND expires_at > now()
		 RETURNING user_id`, codeHash, email).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_CODE",
			"that code is invalid, expired, or already used")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "HASH", err.Error())
		return
	}
	if _, err := h.DB.Exec(r.Context(),
		`UPDATE users SET password_hash=$1, updated_at=now() WHERE id=$2`,
		hash, userID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"verified": true,
		"message":  "Account verified. You can sign in now.",
	})
}

// ---------- POST /auth/me/quota-request (authenticated) --------------------

type quotaReqReq struct {
	RequestedBytes int64  `json:"requestedBytes"`
	Reason         string `json:"reason"`
}

// RequestQuota — the user asks for additional space. Admin gets an email
// and a row in the requests queue.
func (h *AuthHandler) RequestQuota(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	var req quotaReqReq
	if err := jsonBodyOpt(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	if req.RequestedBytes <= 0 {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_REQUEST",
			"requestedBytes must be > 0 (how many MORE bytes you'd like)")
		return
	}

	tag, err := h.DB.Exec(r.Context(), `
		INSERT INTO quota_requests (user_id, requested_bytes, reason)
		VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`,
		u.ID, req.RequestedBytes, strings.TrimSpace(req.Reason))
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusConflict, "PENDING_EXISTS",
			"You already have a pending quota request — wait for that one to be decided before opening another.")
		return
	}
	if h.Mailer != nil && h.AdminEmail != "" {
		_, _ = h.Mailer.SendTemplate(h.AdminEmail,
			"PersonalS3 quota request: "+u.Email,
			"admin_new_quota_request",
			map[string]any{
				"Brand":          "PersonalS3",
				"UserEmail":      u.Email,
				"CurrentHuman":   email.FormatBytes(u.QuotaBytes),
				"RequestedHuman": email.FormatBytes(req.RequestedBytes),
				"Reason":         firstNonEmpty(req.Reason, "(no reason given)"),
				"AdminURL":       h.AdminURL + "/dashboard/admin/requests",
			})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"submitted": true,
		"message":   "Submitted. You'll hear from an administrator by email.",
	})
}

// MyQuotaRequest returns the current user's pending request (if any) so
// the dashboard can show "request pending — N bytes asked".
func (h *AuthHandler) MyQuotaRequest(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	type out struct {
		RequestedBytes int64     `json:"requestedBytes"`
		Reason         string    `json:"reason"`
		RequestedAt    string    `json:"requestedAt"`
	}
	var o out
	err := h.DB.QueryRow(r.Context(), `
		SELECT requested_bytes, reason, to_char(requested_at, 'YYYY-MM-DD"T"HH24:MI:SSOF')
		  FROM quota_requests
		 WHERE user_id = $1 AND status = 'pending'`,
		u.ID,
	).Scan(&o.RequestedBytes, &o.Reason, &o.RequestedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"pending": nil})
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"pending": o})
}

// ---------- helpers --------------------------------------------------------

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func looksLikeEmail(s string) bool {
	i := strings.Index(s, "@")
	return i > 0 && i < len(s)-1 && strings.Contains(s[i+1:], ".")
}

func jsonBodyOpt(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(v)
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// genOTP returns a 6-digit numeric code plus its sha256("<purpose>|<code>")
// hash. We use crypto/rand for entropy + a single math/rand fallback so a
// failure doesn't break onboarding.
func genOTP(purpose string) (code, codeHash string) {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err == nil {
		n := (uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])) % 1000000
		code = fmt.Sprintf("%06d", n)
	} else {
		code = fmt.Sprintf("%06d", mathrand.Intn(1000000))
	}
	return code, hashOTP(purpose, code)
}

func hashOTP(purpose, code string) string {
	sum := sha256.Sum256([]byte(purpose + "|" + code))
	return hex.EncodeToString(sum[:])
}

// CleanupExpiredOTPs is meant to be called periodically (cleaner ticks it).
// Returns the number of rows pruned.
func CleanupExpiredOTPs(ctx context.Context, db *pgxpool.Pool) (int64, error) {
	tag, err := db.Exec(ctx,
		`DELETE FROM otp_codes WHERE consumed_at IS NOT NULL OR expires_at < now() - interval '1 day'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------- POST /auth/me/delete-request (authenticated) -------------------
//
// Step 1 of self-delete: emails the user a one-time code. Doesn't touch
// any data — the user has to come back with the code to actually delete.

func (h *AuthHandler) RequestDelete(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())

	code, codeHash := genOTP("account_delete")
	if _, err := h.DB.Exec(r.Context(), `
		INSERT INTO otp_codes (code_hash, purpose, email, user_id, expires_at)
		VALUES ($1, 'account_delete', $2, $3, now() + interval '10 minutes')`,
		codeHash, u.Email, u.ID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if h.Mailer != nil {
		_, _ = h.Mailer.SendTemplate(u.Email,
			"Confirm deletion of your PersonalS3 account",
			"confirm_delete",
			map[string]any{
				"Brand":        "PersonalS3",
				"Name":         u.Name,
				"Code":         code,
				"TTLMinutes":   otpTTL / 60,
				"SupportEmail": h.supportEmail(),
			})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"sent":    true,
		"message": "We emailed you a one-time code. It expires in 10 minutes.",
	})
}

// ---------- DELETE /auth/me  (authenticated) -------------------------------
//
// Step 2 of self-delete: verify the OTP, then hard-delete the user.
// Cascades remove buckets / objects / api_keys / s3_credentials /
// trusted_devices via FK ON DELETE CASCADE. The cleaner sweeps any
// orphan segments dirs on its next tick.
//
// Admin is emailed for awareness; user gets no follow-up (their inbox
// is already detached from this service).

type deleteSelfReq struct {
	Code string `json:"code"`
}

func (h *AuthHandler) DeleteSelf(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	var req deleteSelfReq
	if err := jsonBodyOpt(r, &req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_CODE",
			"a one-time code is required (request one via POST /auth/me/delete-request)")
		return
	}

	// Atomically consume the OTP — same pattern as password reset.
	codeHash := hashOTP("account_delete", code)
	var matchedUserID uuid.UUID
	err := h.DB.QueryRow(r.Context(), `
		UPDATE otp_codes
		   SET consumed_at = now()
		 WHERE code_hash = $1 AND purpose = 'account_delete'
		   AND user_id = $2 AND email = $3
		   AND consumed_at IS NULL AND expires_at > now()
		 RETURNING user_id`,
		codeHash, u.ID, u.Email).Scan(&matchedUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_CODE",
			"that code is invalid, expired, or already used")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Capture before-delete for the admin notification.
	deletedEmail := u.Email
	ip := clientIP(r)

	// One DELETE — buckets/objects/keys/devices cascade via FK.
	if _, err := h.DB.Exec(r.Context(),
		`DELETE FROM users WHERE id = $1`, u.ID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// Notify admin (best-effort).
	if h.Mailer != nil && h.AdminEmail != "" {
		_, _ = h.Mailer.SendTemplate(h.AdminEmail,
			"PersonalS3 account self-deleted: "+deletedEmail,
			"admin_account_deleted",
			map[string]any{
				"Brand": "PersonalS3",
				"Email": deletedEmail,
				"When":  time.Now().UTC().Format(time.RFC3339),
				"IP":    ip,
			})
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"deleted": true,
		"message": "Your account and all data has been removed.",
	})
}

// clientIP returns just the IP (no port, no proxy chain). The trusted_devices
// table stores it as PostgreSQL INET which rejects "ip:port", so the
// RemoteAddr fallback must be split first.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.Index(v, ","); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
