-- =============================================================================
-- Part 11: Storage management — system-wide configuration
--
-- Simple key/value config so an admin can adjust limits without redeploys.
-- =============================================================================

CREATE TABLE IF NOT EXISTS system_config (
  key         TEXT PRIMARY KEY,
  value       TEXT NOT NULL,
  description TEXT,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Defaults — safe values you can change from the admin dashboard.
INSERT INTO system_config (key, value, description) VALUES
  ('total_quota_bytes',
   '0',
   'System-wide storage cap in bytes. 0 = auto-detect from physical disk minus reserved_bytes.'),

  ('reserved_bytes',
   '5368709120',
   'Bytes reserved on the storage disk for OS / database / logs (5 GiB default). Subtracted from auto-detect.'),

  ('disk_full_threshold_pct',
   '95',
   'Reject new uploads when the physical disk is more than this percentage full.'),

  ('overcommit_allowed',
   'false',
   'If true, allow SUM(user_quotas) to exceed total_quota_bytes. Useful when most users underutilize.')

ON CONFLICT (key) DO NOTHING;
