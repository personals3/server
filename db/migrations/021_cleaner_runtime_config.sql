-- Cleaner runtime tunables move from .env to system_config so the admin
-- panel can flip them without rebuilding the container.
--
-- Cleaner re-reads these every tick (cheap single-row queries), so a
-- change in the dashboard takes effect within ~30s — no restart needed.

INSERT INTO system_config (key, value, description) VALUES
  ('cleanup_dry_run',           'false',
     'When true, the cleaner logs orphan candidates but never deletes. Useful for auditing before flipping to live mode.'),
  ('orphan_two_strike',          'true',
     'When true, a path must appear orphaned on TWO consecutive cleaner ticks before deletion. Safer; recommended in production.'),
  ('orphan_min_age_minutes',     '30',
     'Files newer than this are exempt from orphan reaping (race protection for in-flight uploads). 0 disables the gate — only useful in tests.'),
  ('bloom_rebuild_hours',        '6',
     'How often the legacy bloom-membership filter rebuilds. Longer = cheaper but more stale; shorter = more authoritative but more DB load.'),
  ('cleanup_interval_seconds',   '30',
     'Cleaner tick interval. Lower = faster orphan detection, higher = less DB churn.')
ON CONFLICT (key) DO NOTHING;
