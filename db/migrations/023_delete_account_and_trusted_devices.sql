-- =============================================================================
-- Two additions for self-serve account management:
--
--   1. account_delete OTP purpose — users can self-delete after confirming
--      via a one-time code mailed to their address. We reuse the existing
--      otp_codes table; just extend the CHECK constraint.
--
--   2. trusted_devices — once a 2FA-enabled user passes a TOTP challenge,
--      they can opt to "trust this device for 30 days". The server mints
--      a random token (stored hashed) and the browser sends it on every
--      subsequent login as `X-Trusted-Device`. If valid, the 2FA prompt
--      is skipped. Capped at 3 devices per user — the oldest is evicted
--      when a 4th is added.
-- =============================================================================

-- ---- Extend OTP purposes ----------------------------------------------------

ALTER TABLE otp_codes DROP CONSTRAINT IF EXISTS otp_codes_purpose_check;
ALTER TABLE otp_codes ADD CONSTRAINT otp_codes_purpose_check
  CHECK (purpose IN (
    'account_verification',
    'password_reset',
    'account_delete'
  ));

-- ---- Trusted devices --------------------------------------------------------

CREATE TABLE IF NOT EXISTS trusted_devices (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  -- sha256("v1|" || raw_token). Raw token (32 bytes hex) is sent to the
  -- browser exactly once at creation; we never store it. A leaked DB
  -- doesn't yield reusable tokens.
  token_hash   TEXT NOT NULL UNIQUE,
  -- Short human-friendly name the user can edit. Auto-derived from UA
  -- on creation (e.g. "Chrome on Mac" / "Firefox on Linux").
  label        TEXT NOT NULL DEFAULT '',
  -- Best-effort fingerprint metadata. Informational only; we don't gate
  -- on them. If the user moves networks the token still works.
  ip_address   INET,
  user_agent   TEXT,
  -- Trust window. We cap at 30 days at issue time; refresh extends.
  expires_at   TIMESTAMPTZ NOT NULL,
  -- Updated every time the device is used to skip 2FA. Eviction policy
  -- (when a user exceeds the per-user cap) picks the row with the
  -- oldest last_used_at.
  last_used_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_trusted_devices_user
  ON trusted_devices (user_id, last_used_at DESC);

CREATE INDEX IF NOT EXISTS idx_trusted_devices_cleanup
  ON trusted_devices (expires_at);
