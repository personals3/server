-- =============================================================================
-- Part 10: S3-style credentials (AWS Access Key ID + Secret Access Key)
--
-- These are separate from api_keys (which are Bearer tokens) — they sit in
-- a different namespace so AWS SDKs can use SigV4 against the same backend
-- without conflicting with the dashboard's Bearer-token API key format.
-- =============================================================================

CREATE TABLE IF NOT EXISTS s3_credentials (
  access_key_id     TEXT PRIMARY KEY,                           -- "AKIA..." style, 20 chars
  user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  secret_key        TEXT NOT NULL,                              -- stored in plaintext —
                                                                -- we need it for HMAC
  name              TEXT,
  last_used_at      TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_s3_credentials_user ON s3_credentials(user_id);

-- NOTE on plaintext storage: SigV4 verification requires HMACing the request
-- with the original secret. Unlike a password hash (where we only need to
-- verify a presented password), we must regenerate the signing key from
-- the secret on every request. This is the same trade-off AWS makes
-- internally — IAM stores secret_access_key encrypted at rest but accessible
-- to the SigV4 verifier.
--
-- Mitigation: keep these credentials separate from password hashes, give
-- users a way to rotate them easily.
