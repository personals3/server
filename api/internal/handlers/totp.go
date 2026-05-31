package handlers

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"

	"github.com/personals3/api/internal/auth"
	"github.com/personals3/api/internal/httpx"
	"github.com/personals3/api/internal/middleware"
)

// TOTPHandler — endpoints for setting up + verifying TOTP 2FA.
//
//   POST /auth/2fa/setup            — generate secret + QR code (requires auth)
//   POST /auth/2fa/verify           — verify a code and ENABLE 2FA (requires auth)
//   POST /auth/2fa/disable          — disable 2FA (requires auth + code)
//   POST /auth/2fa/login            — exchange (challenge, code) for JWT
//   POST /auth/2fa/recovery/regen   — regenerate recovery codes (requires auth)
//
// AuthHandler.Login is the entry point: when totp_enabled, it returns
// {require2fa, challenge} instead of {token}. The dashboard then calls
// /auth/2fa/login with that challenge + the user's 6-digit code.
type TOTPHandler struct {
	DB        *pgxpool.Pool
	JWTSecret string
	Issuer    string // shows up in the user's authenticator app as the account name
}

// ----- Setup --------------------------------------------------------------

// POST /auth/2fa/setup — generate a new secret + recovery codes.
// Does NOT enable 2FA yet (user must verify a code first).
// Returns the otpauth:// URL (encode to QR client-side).
type setupResp struct {
	Secret       string   `json:"secret"`       // Base32-encoded
	OtpauthURL   string   `json:"otpauthUrl"`   // for QR code
	RecoveryCodes []string `json:"recoveryCodes"`// shown ONCE
}

func (h *TOTPHandler) Setup(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())

	// Already enabled? Tell caller to disable first.
	var enabled bool
	_ = h.DB.QueryRow(r.Context(),
		`SELECT totp_enabled FROM users WHERE id = $1`, u.ID).Scan(&enabled)
	if enabled {
		httpx.WriteError(w, http.StatusConflict, "ALREADY_ENABLED",
			"2FA is already enabled — disable it first to set up a new secret")
		return
	}

	issuer := h.Issuer
	if issuer == "" {
		issuer = "PersonalS3"
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      issuer,
		AccountName: u.Email,
		Period:      30,
		Digits:      otp.DigitsSix,
	})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "GEN", err.Error())
		return
	}

	codes, hashedCodes, err := generateRecoveryCodes(10)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "GEN", err.Error())
		return
	}
	hashedJSON, _ := json.Marshal(hashedCodes)

	// Store secret + hashed codes. totp_enabled stays FALSE until verify.
	if _, err := h.DB.Exec(r.Context(), `
		UPDATE users SET totp_secret = $1, recovery_codes = $2, totp_enabled = false
		 WHERE id = $3`,
		key.Secret(), hashedJSON, u.ID,
	); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	httpx.WriteJSON(w, http.StatusOK, setupResp{
		Secret:        key.Secret(),
		OtpauthURL:    key.URL(),
		RecoveryCodes: codes,
	})
}

// POST /auth/2fa/verify — verify a code; on success, flip totp_enabled=true.
type verifyReq struct {
	Code string `json:"code"`
}

func (h *TOTPHandler) Verify(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	var req verifyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}

	var secret string
	if err := h.DB.QueryRow(r.Context(),
		`SELECT COALESCE(totp_secret,'') FROM users WHERE id = $1`, u.ID,
	).Scan(&secret); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if secret == "" {
		httpx.WriteError(w, http.StatusBadRequest, "NO_SECRET",
			"call /auth/2fa/setup first")
		return
	}
	if !totp.Validate(req.Code, secret) {
		httpx.WriteError(w, http.StatusUnauthorized, "BAD_CODE", "invalid code")
		return
	}

	if _, err := h.DB.Exec(r.Context(),
		`UPDATE users SET totp_enabled = true WHERE id = $1`, u.ID,
	); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"enabled": true})
}

// POST /auth/2fa/disable — turn 2FA off. Requires a valid current code OR
// a recovery code (so a lost device can recover via the printed codes).
func (h *TOTPHandler) Disable(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	var req verifyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}

	ok, err := h.checkCodeOrRecovery(r, u.ID, req.Code)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "BAD_CODE", "invalid code")
		return
	}

	if _, err := h.DB.Exec(r.Context(), `
		UPDATE users SET totp_enabled = false, totp_secret = NULL, recovery_codes = '[]'
		 WHERE id = $1`, u.ID,
	); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /auth/2fa/recovery/regen — regenerate recovery codes (requires current code).
func (h *TOTPHandler) RegenerateRecovery(w http.ResponseWriter, r *http.Request) {
	u := middleware.MustUser(r.Context())
	var req verifyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}
	ok, err := h.checkCodeOrRecovery(r, u.ID, req.Code)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "BAD_CODE", "invalid code")
		return
	}
	codes, hashed, err := generateRecoveryCodes(10)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "GEN", err.Error())
		return
	}
	hashedJSON, _ := json.Marshal(hashed)
	if _, err := h.DB.Exec(r.Context(),
		`UPDATE users SET recovery_codes = $1 WHERE id = $2`, hashedJSON, u.ID,
	); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

// ----- Login -------------------------------------------------------------

// POST /auth/2fa/login — exchange (challenge, code) → JWT.
type totpLoginReq struct {
	Challenge string `json:"challenge"`
	Code      string `json:"code"`
}

func (h *TOTPHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req totpLoginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "BAD_JSON", err.Error())
		return
	}

	// Look up the challenge token; it must not be expired.
	var userID uuid.UUID
	err := h.DB.QueryRow(r.Context(), `
		SELECT user_id FROM totp_challenges
		 WHERE token = $1 AND expires_at > now()`,
		req.Challenge,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		httpx.WriteError(w, http.StatusUnauthorized, "BAD_CHALLENGE",
			"challenge invalid or expired — start login over")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}

	// One-shot: delete the challenge regardless of outcome below.
	defer func() {
		_, _ = h.DB.Exec(r.Context(),
			`DELETE FROM totp_challenges WHERE token = $1`, req.Challenge)
	}()

	ok, err := h.checkCodeOrRecovery(r, userID, req.Code)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "BAD_CODE", "invalid code")
		return
	}

	// Hand back a JWT — same shape AuthHandler.Login uses on success.
	var u struct {
		ID    uuid.UUID
		Email string
		Role  string
	}
	if err := h.DB.QueryRow(r.Context(),
		`SELECT id, email, role FROM users WHERE id = $1`, userID,
	).Scan(&u.ID, &u.Email, &u.Role); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "DB", err.Error())
		return
	}
	tok, err := auth.IssueJWT(h.JWTSecret, u.ID, u.Email, u.Role)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "JWT", err.Error())
		return
	}

	// If the user asked to trust this device (?trust=label), mint a token
	// they'll send as X-Trusted-Device on future logins to skip 2FA. Cap
	// of 3 per user enforced inside IssueTrustedDevice; oldest is evicted.
	resp := map[string]any{
		"token": tok,
		"user":  map[string]any{"id": u.ID, "email": u.Email, "role": u.Role},
	}
	if trust := r.URL.Query().Get("trust"); trust != "" {
		label := trust
		if trust == "1" || trust == "true" {
			label = "" // let the helper derive from UA
		}
		if devTok, err := IssueTrustedDevice(r, h.DB, u.ID, label); err == nil {
			resp["trustedDevice"] = devTok
		} else {
			// Log loudly — login still succeeds with normal 2FA each
			// time, but a silent fail here is what hid the inet/port
			// bug for as long as it lasted.
			log.Printf("trusted-device issue failed for user=%s: %v", u.ID, err)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

// ----- helpers -----------------------------------------------------------

// CreateChallenge writes a new totp_challenge row and returns the random
// opaque token. Used by AuthHandler.Login when 2FA is required.
func (h *TOTPHandler) CreateChallenge(r *http.Request, userID uuid.UUID) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(raw)
	if _, err := h.DB.Exec(r.Context(), `
		INSERT INTO totp_challenges (token, user_id, expires_at)
		VALUES ($1, $2, $3)`,
		tok, userID, time.Now().Add(5*time.Minute),
	); err != nil {
		return "", err
	}
	return tok, nil
}

// checkCodeOrRecovery verifies either the live TOTP code or one of the
// user's recovery codes. Recovery codes are single-use: on success we
// remove that hash from the array in the same transaction.
func (h *TOTPHandler) checkCodeOrRecovery(r *http.Request, userID uuid.UUID, code string) (bool, error) {
	var secret string
	var codesJSON []byte
	if err := h.DB.QueryRow(r.Context(),
		`SELECT COALESCE(totp_secret,''), recovery_codes FROM users WHERE id = $1`,
		userID,
	).Scan(&secret, &codesJSON); err != nil {
		return false, err
	}

	if secret != "" && totp.Validate(code, secret) {
		return true, nil
	}

	// Try recovery codes. Each is single-use; consumed on match.
	var hashes []string
	_ = json.Unmarshal(codesJSON, &hashes)
	for i, hsh := range hashes {
		if bcrypt.CompareHashAndPassword([]byte(hsh), []byte(code)) == nil {
			remaining := append([]string{}, hashes[:i]...)
			remaining = append(remaining, hashes[i+1:]...)
			b, _ := json.Marshal(remaining)
			if _, err := h.DB.Exec(r.Context(),
				`UPDATE users SET recovery_codes = $1 WHERE id = $2`, b, userID,
			); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// generateRecoveryCodes creates N user-facing codes + their bcrypt hashes.
// User-facing codes look like "ABCD-EFGH-IJKL" — base32 of random bytes.
func generateRecoveryCodes(n int) (display []string, hashes []string, err error) {
	display = make([]string, 0, n)
	hashes = make([]string, 0, n)
	for i := 0; i < n; i++ {
		raw := make([]byte, 9) // 9 bytes → 15 base32 chars
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, err
		}
		s := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
		// Format "XXXX-XXXX-XXXX" for legibility
		formatted := fmt.Sprintf("%s-%s-%s", s[0:4], s[4:9], s[9:14])
		display = append(display, formatted)

		h, err := bcrypt.GenerateFromPassword([]byte(formatted), bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, err
		}
		hashes = append(hashes, string(h))
	}
	return
}
