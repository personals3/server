-- =============================================================================
-- Two admin-tunable capacity knobs.
--
-- Cleaner reads system_config every tick; the API checks these on every
-- signup / approval / login. Flip them in the dashboard and the change
-- takes effect on the next request — no rebuild, no env edit.
--
--   max_users          — Maximum active accounts. New requests beyond
--                        this auto-deny; existing users beyond this
--                        soft-block at login until the cap rises or
--                        seats free up. 0 = unlimited (escape hatch).
--
--   max_allocation_pct — Percentage of disk that the SUM of all user
--                        quotas may consume. Defaults to 90 so there's
--                        always ≥10% headroom for transcoded segments,
--                        system files, and in-flight write surges.
--                        overcommit_allowed=true bypasses (you already
--                        have that flag for the rare "I trust nobody
--                        will fill their quota" case).
-- =============================================================================

INSERT INTO system_config (key, value, description) VALUES
  ('max_users', '100',
     'Maximum active user accounts. 0 = unlimited. New requests beyond this auto-deny; existing users beyond this can''t sign in until the cap is raised or others are removed.'),
  ('max_allocation_pct', '90',
     'Percentage of disk that can be allocated across all user quotas (1-100). Approvals that would exceed this are rejected. overcommit_allowed=true bypasses.')
ON CONFLICT (key) DO NOTHING;
