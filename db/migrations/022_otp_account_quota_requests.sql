-- =============================================================================
-- Self-serve onboarding + ops surface for a multi-user PersonalS3 instance.
--
-- Three new tables and one column:
--
--   otp_codes          — short-lived one-time codes for account verification,
--                        password reset. Stored hashed, single-use, 10-min TTL.
--
--   account_requests   — "I'd like an account" form a stranger fills in.
--                        Admin approves → user is created with a default
--                        100 MB quota + an email-verification OTP.
--
--   quota_requests     — "I need more than 100 MB" form an existing user
--                        fills in. Admin approves → bumps quota_bytes and
--                        emails them.
--
--   api_keys.scope     — placeholder for v2: per-key permission scoping
--                        ('full' today, future: read / write / share-only /
--                        admin). Wired through middleware but enforces only
--                        'full' for now so behaviour is unchanged.
-- =============================================================================

-- ---- OTPs --------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS otp_codes (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  code_hash    TEXT NOT NULL,            -- sha256 of "purpose|code"
  purpose      TEXT NOT NULL CHECK (purpose IN (
                  'account_verification',
                  'password_reset')),
  -- For password_reset we have an actual user. For account_verification we
  -- might still be in the "request approved, account not created yet" state
  -- so email is enough.
  email        TEXT NOT NULL,
  user_id      UUID REFERENCES users(id) ON DELETE CASCADE,
  expires_at   TIMESTAMPTZ NOT NULL,
  consumed_at  TIMESTAMPTZ,              -- single-use flag
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Throttle: at most a handful of issuances per email per hour
  CONSTRAINT otp_codes_email_purpose_lookup UNIQUE (email, purpose, created_at)
);

CREATE INDEX IF NOT EXISTS idx_otp_codes_lookup
  ON otp_codes (email, purpose, consumed_at)
  WHERE consumed_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_otp_codes_cleanup
  ON otp_codes (expires_at)
  WHERE consumed_at IS NULL;

-- ---- Account requests --------------------------------------------------------

CREATE TABLE IF NOT EXISTS account_requests (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email          TEXT NOT NULL,
  name           TEXT NOT NULL,
  reason         TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'approved', 'denied')),
  -- For approved requests: what quota we granted. Lets the admin override
  -- the 100 MB default per-request without touching every user row later.
  granted_quota_bytes BIGINT,
  admin_note     TEXT NOT NULL DEFAULT '',
  requested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  decided_at     TIMESTAMPTZ,
  decided_by     UUID REFERENCES users(id) ON DELETE SET NULL,
  -- Once approved + user created, points at the user row. Lets us avoid
  -- creating duplicate users if an admin clicks Approve twice.
  created_user_id UUID REFERENCES users(id) ON DELETE SET NULL
);

-- A pending request from a given email is unique — no double-submits.
CREATE UNIQUE INDEX IF NOT EXISTS idx_account_requests_one_pending
  ON account_requests (lower(email))
  WHERE status = 'pending';

-- ---- Quota requests ----------------------------------------------------------

CREATE TABLE IF NOT EXISTS quota_requests (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  requested_bytes BIGINT NOT NULL CHECK (requested_bytes > 0),
  reason          TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'approved', 'denied')),
  granted_bytes   BIGINT,                -- nullable; admin may grant less than asked
  admin_note      TEXT NOT NULL DEFAULT '',
  requested_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  decided_at      TIMESTAMPTZ,
  decided_by      UUID REFERENCES users(id) ON DELETE SET NULL
);

-- One pending quota request per user — they have to wait for the previous
-- decision before opening another one.
CREATE UNIQUE INDEX IF NOT EXISTS idx_quota_requests_one_pending
  ON quota_requests (user_id)
  WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS idx_quota_requests_history
  ON quota_requests (user_id, requested_at DESC);

-- ---- api_keys.scope (forward-compat) ----------------------------------------

ALTER TABLE api_keys
  ADD COLUMN IF NOT EXISTS scope TEXT NOT NULL DEFAULT 'full'
    CHECK (scope IN ('full', 'read', 'write', 'share-only', 'admin'));

-- ---- Default quota bump ------------------------------------------------------

-- The Go config default still wins on fresh deploys; this only bumps
-- existing rows that have the old "10 GB per user, no questions asked"
-- value. New deployments running on top of an existing 10 GB default
-- can leave their users alone — we explicitly set quota at approval time.
-- (Commented out — we don't want to silently nerf existing users.)
-- UPDATE users SET quota_bytes = 104857600
--   WHERE quota_bytes = 10737418240;
