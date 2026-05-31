-- =============================================================================
-- Share links — tracks every presigned URL the server issues so the owner
-- can list, extend/shorten, or revoke them. Without this table the URL was
-- a pure HMAC signature with no server-side state, meaning revocation was
-- impossible short of rotating the JWT secret.
--
-- Authority model:
--   - The HMAC signature continues to verify tamper-proofness (the URL
--     can't be forged or modified)
--   - The DB row is the AUTHORITATIVE expires_at — so we can extend or
--     shorten without re-issuing
--   - revoked=TRUE makes ServePresigned reject the link immediately
--
-- We key on sha256(sig) instead of the raw sig so the URL is the credential
-- and the DB lookup is constant-time.
-- =============================================================================

CREATE TABLE IF NOT EXISTS share_links (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  bucket_name    TEXT NOT NULL,    -- denormalized so revocation survives bucket rename
  object_key     TEXT NOT NULL,
  method         TEXT NOT NULL CHECK (method IN ('GET','HEAD','PUT')),
  sig_hash       BYTEA NOT NULL,   -- sha256(sig) — the lookup key
  expires_at     TIMESTAMPTZ NOT NULL,
  force_download BOOLEAN NOT NULL DEFAULT false,
  revoked        BOOLEAN NOT NULL DEFAULT false,
  revoked_at     TIMESTAMPTZ,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at   TIMESTAMPTZ,
  use_count      INT NOT NULL DEFAULT 0
);

-- Lookup by signature hash on every share request — must be O(1).
CREATE UNIQUE INDEX IF NOT EXISTS idx_share_links_sig
  ON share_links(sig_hash);

-- Listing user's active shares.
CREATE INDEX IF NOT EXISTS idx_share_links_owner
  ON share_links(owner_id, created_at DESC);
