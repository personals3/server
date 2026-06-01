-- Persist quota events so admins can audit historical charges/refunds/rejections
-- instead of only catching them in the live SSE tail.
--
-- One row per QuotaReserve call. Rows accrue fast (every upload, every share,
-- every transcode reservation), so retention is on the cleaner's plate.

CREATE TABLE IF NOT EXISTS quota_events (
    id          BIGSERIAL    PRIMARY KEY,
    ts          TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    user_id     UUID         NOT NULL,
    delta       BIGINT       NOT NULL,           -- +charge / -refund
    new_bytes   BIGINT       NOT NULL DEFAULT 0, -- post-update used_bytes (0 on rejected)
    caller      TEXT         NOT NULL,           -- file:line that called QuotaReserve
    rejected    BOOLEAN      NOT NULL DEFAULT FALSE
);

-- Paginated reads always order by ts DESC.
CREATE INDEX IF NOT EXISTS quota_events_ts_idx ON quota_events (ts DESC);

-- Filter by user is common when investigating a single account.
CREATE INDEX IF NOT EXISTS quota_events_user_ts_idx ON quota_events (user_id, ts DESC);
