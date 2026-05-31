-- =============================================================================
-- TOTP-based two-factor authentication.
--
-- Columns:
--   totp_secret       — Base32-encoded shared secret, set during setup.
--                       NULL means 2FA was never configured (or was disabled).
--   totp_enabled      — TRUE only after the user verified a code, locking it in.
--   recovery_codes    — JSON array of bcrypt hashes for one-time recovery codes
--                       (10 codes generated at setup; consumed on use).
--
-- Login flow:
--   1. POST /auth/login → if totp_enabled, returns {require2fa: true, challenge: <opaque>}
--      instead of a JWT
--   2. POST /auth/2fa/login with {challenge, code} → returns JWT
--
-- Recovery codes:
--   - Shown ONCE at setup
--   - Hashed (bcrypt) before storing
--   - Each can be used at most once; consumed on use
--   - User can regenerate (invalidates all existing) via /auth/2fa/recovery/regen
-- =============================================================================

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS totp_secret    TEXT,
  ADD COLUMN IF NOT EXISTS totp_enabled   BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS recovery_codes JSONB NOT NULL DEFAULT '[]';

-- Short-lived "2FA pending" challenges, keyed by random opaque token.
-- Lives in DB so it survives API restarts (preferred over Valkey for this
-- because the table is tiny and we want it persistent for forensics).
CREATE TABLE IF NOT EXISTS totp_challenges (
  token       TEXT PRIMARY KEY,
  user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ NOT NULL DEFAULT (now() + INTERVAL '5 minutes')
);

CREATE INDEX IF NOT EXISTS idx_totp_challenges_expires
  ON totp_challenges(expires_at)
  WHERE expires_at IS NOT NULL;
