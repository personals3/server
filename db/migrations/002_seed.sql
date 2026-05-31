-- =============================================================================
-- Seed: create initial admin user from env (ADMIN_EMAIL / ADMIN_PASSWORD).
--
-- The credentials are forwarded into Postgres via PGOPTIONS GUCs from the
-- postgres service in docker-compose.yml. If either is missing, this seed is
-- a no-op — an operator must create the admin out-of-band rather than have
-- a guessable default account silently materialise.
--
-- pgcrypto's crypt(password, gen_salt('bf', 10)) produces an OpenBSD-format
-- bcrypt hash compatible with Go's golang.org/x/crypto/bcrypt.
-- =============================================================================

DO $$
DECLARE
  email TEXT := NULLIF(current_setting('app.admin_email', true), '');
  pw    TEXT := NULLIF(current_setting('app.admin_password', true), '');
BEGIN
  IF email IS NULL OR pw IS NULL THEN
    RAISE NOTICE 'seed: ADMIN_EMAIL/ADMIN_PASSWORD not set, skipping admin bootstrap';
    RETURN;
  END IF;

  INSERT INTO users (email, name, password_hash, role, quota_bytes)
  VALUES (
    email,
    'Administrator',
    crypt(pw, gen_salt('bf', 10)),
    'admin',
    1099511627776  -- 1 TB
  )
  ON CONFLICT (email) DO NOTHING;
END $$;
