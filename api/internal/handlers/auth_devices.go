// Trusted-device endpoints.
//
// Lifecycle:
//
//   1. User signs in with password → 2FA challenge → POSTs code to
//      /auth/2fa/login WITH ?trust=this-device-label in the query.
//   2. /auth/2fa/login (in totp.go) mints a 32-byte random token,
//      stores sha256("v1|"||token) + metadata in trusted_devices, and
//      returns the raw token alongside the JWT.
//   3. Browser persists the raw token in localStorage.
//   4. Subsequent /auth/login attempts include `X-Trusted-Device: <token>`
//      and bypass the 2FA challenge if the hash matches an unexpired row.
//
// Per-user cap = 3. Adding a 4th evicts the row with the oldest
// last_used_at. Password reset wipes all of a user's devices.

package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

const (
	trustedDeviceTTL = 30 * 24 * time.Hour
	trustedDeviceMax = 3
)

// consumeTrustedDevice validates `token` against the user's stored
// trusted-device rows. Returns true and bumps last_used_at on a match.
// Side-effect free on miss.
func (h *AuthHandler) consumeTrustedDevice(r *http.Request, userID uuid.UUID, token string) bool {
	if token == "" {
		return false
	}
	hash := hashDeviceToken(token)
	tag, err := h.DB.Exec(r.Context(), `
		UPDATE trusted_devices
		   SET last_used_at = now()
		 WHERE user_id = $1 AND token_hash = $2 AND expires_at > now()`,
		userID, hash)
	return err == nil && tag.RowsAffected() == 1
}

// IssueTrustedDevice creates a row + returns the raw token (caller MUST
// send it to the user — DB only has the hash). Enforces the per-user cap.
// Free function so both AuthHandler and TOTPHandler can call it without
// crossing struct boundaries.
func IssueTrustedDevice(r *http.Request, db *pgxpool.Pool, userID uuid.UUID, label string) (string, error) {
	raw, err := newDeviceToken()
	if err != nil {
		return "", err
	}
	hash := hashDeviceToken(raw)

	if strings.TrimSpace(label) == "" {
		label = deriveLabelFromUA(r.UserAgent())
	}
	ip := clientIP(r)

	tx, err := db.Begin(r.Context())
	if err != nil {
		return "", err
	}
	defer tx.Rollback(r.Context())

	// Dedupe: if THIS browser/device already has a trusted-device row
	// (matched by user_agent), drop the old one before issuing a new token.
	// This is the recovery path for users who lost their browser data —
	// they re-trust the same physical device on next login, and we don't
	// want a stale "ghost" row sitting in the list eating one of the 3 slots.
	// We match by user_agent exact-equality. IP would be too brittle (DHCP,
	// VPN, mobile) and a user-supplied label would be even less reliable.
	ua := r.UserAgent()
	if ua != "" {
		if _, err := tx.Exec(r.Context(),
			`DELETE FROM trusted_devices WHERE user_id = $1 AND user_agent = $2`,
			userID, ua,
		); err != nil {
			return "", err
		}
	}

	// Evict the oldest device first if at cap. Picking by last_used_at
	// keeps frequently-used devices alive. (Runs only when dedupe didn't
	// already make room.)
	var count int
	if err := tx.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM trusted_devices WHERE user_id = $1`, userID,
	).Scan(&count); err != nil {
		return "", err
	}
	if count >= trustedDeviceMax {
		if _, err := tx.Exec(r.Context(), `
			DELETE FROM trusted_devices
			 WHERE id = (
				SELECT id FROM trusted_devices
				 WHERE user_id = $1
				 ORDER BY last_used_at ASC
				 LIMIT 1)`, userID); err != nil {
			return "", err
		}
	}

	// Compute expiry in Go and pass a timestamp directly — earlier we
	// tried `now() + ($6 || ' seconds')::interval` but pgx refused to
	// encode an int64 into text. A single timestamptz round-trips cleanly.
	expiresAt := time.Now().Add(trustedDeviceTTL)
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO trusted_devices (user_id, token_hash, label, ip_address, user_agent, expires_at)
		VALUES ($1, $2, $3, NULLIF($4, '')::inet, $5, $6)`,
		userID, hash, label, ip, r.UserAgent(), expiresAt,
	); err != nil {
		return "", err
	}
	if err := tx.Commit(r.Context()); err != nil {
		return "", err
	}
	return raw, nil
}


// ---------- GET /auth/me/devices --------------------------------------------

type deviceDTO struct {
	ID         uuid.UUID `json:"id"`
	Label      string    `json:"label"`
	IPAddress  string    `json:"ipAddress"`
	UserAgent  string    `json:"userAgent"`
	LastUsedAt time.Time `json:"lastUsedAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	CreatedAt  time.Time `json:"createdAt"`
}

func (h *AuthHandler) ListDevices(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	rows, err := h.DB.Query(r.Context(), `
		SELECT id, label, COALESCE(host(ip_address)::text, ''),
		       COALESCE(user_agent, ''),
		       last_used_at, expires_at, created_at
		  FROM trusted_devices
		 WHERE user_id = $1 AND expires_at > now()
		 ORDER BY last_used_at DESC`, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	defer rows.Close()
	out := []deviceDTO{}
	for rows.Next() {
		var d deviceDTO
		if err := rows.Scan(&d.ID, &d.Label, &d.IPAddress, &d.UserAgent,
			&d.LastUsedAt, &d.ExpiresAt, &d.CreatedAt); err != nil {
			continue
		}
		out = append(out, d)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"devices": out,
		"max":     trustedDeviceMax,
	})
}

// ---------- DELETE /auth/me/devices/{id} ------------------------------------

func (h *AuthHandler) RevokeDevice(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_ID", "invalid device id")
		return
	}
	tag, err := h.DB.Exec(r.Context(),
		`DELETE FROM trusted_devices WHERE id = $1 AND user_id = $2`, id, u.ID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		httpx.WriteError(w, http.StatusNotFound, "NO_SUCH_DEVICE", "device not found or not yours")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------- helpers ---------------------------------------------------------

func newDeviceToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func hashDeviceToken(token string) string {
	sum := sha256.Sum256([]byte("v1|" + token))
	return hex.EncodeToString(sum[:])
}

// deriveLabelFromUA produces a short human-readable label from a User-Agent
// string. Not bulletproof — just enough so users can tell their devices
// apart in the list. They can rename later.
func deriveLabelFromUA(ua string) string {
	ua = strings.ToLower(ua)
	browser := "Browser"
	switch {
	case strings.Contains(ua, "firefox"):
		browser = "Firefox"
	case strings.Contains(ua, "edg/"):
		browser = "Edge"
	case strings.Contains(ua, "chrome"):
		browser = "Chrome"
	case strings.Contains(ua, "safari"):
		browser = "Safari"
	}
	os := "device"
	switch {
	case strings.Contains(ua, "android"):
		os = "Android"
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"):
		os = "iOS"
	case strings.Contains(ua, "mac os"), strings.Contains(ua, "macintosh"):
		os = "Mac"
	case strings.Contains(ua, "linux"):
		os = "Linux"
	case strings.Contains(ua, "windows"):
		os = "Windows"
	}
	return browser + " on " + os
}

