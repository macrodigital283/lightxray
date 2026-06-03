-- lightxray panel schema. One DB per panel; migrated on startup via Migrate().
--
-- Conventions:
--   - times stored as TIMESTAMPTZ; converted to Hiddify's UTC string at
--     the JSON boundary by internal/util.HiddifyTime.
--   - traffic counters stored as bytes (BIGINT); converted to GB float
--     at the JSON boundary by internal/util.BytesToGB.

CREATE TABLE IF NOT EXISTS users (
    uuid              UUID        PRIMARY KEY,
    name              TEXT        NOT NULL,
    comment           TEXT        NOT NULL DEFAULT '',

    -- traffic (monotonic, summed across xray restarts via stats_cursor below)
    usage_bytes       BIGINT      NOT NULL DEFAULT 0,
    usage_limit_bytes BIGINT      NOT NULL DEFAULT 0,  -- 0 = unlimited

    -- lifecycle
    package_days      INTEGER     NOT NULL DEFAULT 0,  -- 0 = unlimited
    start_date        DATE,                            -- null until first connect
    last_reset_time   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_online       TIMESTAMPTZ,                     -- null = never connected
    enable            BOOLEAN     NOT NULL DEFAULT TRUE,
    mode              TEXT        NOT NULL DEFAULT 'no_reset',
    lang              TEXT        NOT NULL DEFAULT 'en',

    -- bookkeeping
    added_by_uuid     UUID        NOT NULL,
    serial_id         BIGSERIAL   UNIQUE,              -- Hiddify exposes as `id`
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS users_last_online_idx ON users (last_online);
CREATE INDEX IF NOT EXISTS users_enable_idx      ON users (enable);
CREATE INDEX IF NOT EXISTS users_serial_idx      ON users (serial_id);

-- CDN-fronted hostnames the subscription bundle emits one vless URL
-- per. Operators add/remove these via the dashboard; the LX_CDN_HOSTS
-- env var (if set) seeds the table on first startup but isn't the
-- source of truth at runtime.
CREATE TABLE IF NOT EXISTS cdn_hosts (
    id         BIGSERIAL    PRIMARY KEY,
    hostname   TEXT         UNIQUE NOT NULL,
    enabled    BOOLEAN      NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS cdn_hosts_enabled_idx ON cdn_hosts (enabled);

-- Per-host transport pick. Allowed: 'ws', 'grpc', 'both'. Added in a
-- separate ALTER so older deployments upgrade in place.
ALTER TABLE cdn_hosts ADD COLUMN IF NOT EXISTS transport TEXT NOT NULL DEFAULT 'ws';
ALTER TABLE cdn_hosts DROP CONSTRAINT IF EXISTS cdn_hosts_transport_chk;
ALTER TABLE cdn_hosts ADD CONSTRAINT cdn_hosts_transport_chk
    CHECK (transport IN ('ws', 'grpc', 'both'));

-- xray's StatsService counters are monotonic from process start. To compute
-- a delta across an xray restart we persist the last value we observed; if
-- the next read is LOWER, we treat the delta as new (the counter reset).
CREATE TABLE IF NOT EXISTS stats_cursor (
    uuid           UUID        PRIMARY KEY REFERENCES users(uuid) ON DELETE CASCADE,
    last_uplink    BIGINT      NOT NULL DEFAULT 0,
    last_downlink  BIGINT      NOT NULL DEFAULT 0,
    last_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
