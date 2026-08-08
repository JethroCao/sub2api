-- Audited administrator operations, immutable upstream-cost snapshots, and
-- bounded video operations metrics. This migration is forward-only and does
-- not rewrite any previously applied unified-video migration.

ALTER TABLE video_pricing_rules
    DROP CONSTRAINT IF EXISTS video_pricing_rules_finite_nonnegative_prices;

ALTER TABLE video_pricing_rules
    ADD CONSTRAINT video_pricing_rules_finite_nonnegative_prices CHECK (
        unit_price >= 0 AND unit_price < 'Infinity'::numeric
        AND (
            upstream_unit_cost IS NULL
            OR (upstream_unit_cost >= 0 AND upstream_unit_cost < 'Infinity'::numeric)
        )
    );

ALTER TABLE video_tasks
    ADD COLUMN IF NOT EXISTS upstream_unit_cost DECIMAL(20,10);

ALTER TABLE video_tasks
    DROP CONSTRAINT IF EXISTS video_tasks_upstream_unit_cost_finite;

ALTER TABLE video_tasks
    ADD CONSTRAINT video_tasks_upstream_unit_cost_finite CHECK (
        upstream_unit_cost IS NULL
        OR (upstream_unit_cost >= 0 AND upstream_unit_cost < 'Infinity'::numeric)
    );

CREATE TABLE IF NOT EXISTS video_admin_actions (
    id                   BIGSERIAL PRIMARY KEY,
    request_id           VARCHAR(36) NOT NULL REFERENCES video_tasks(request_id) ON DELETE CASCADE,
    action               VARCHAR(16) NOT NULL CHECK (action IN ('reconcile', 'refund', 'complete')),
    idempotency_key_hash VARCHAR(64) NOT NULL CHECK (idempotency_key_hash ~ '^[0-9a-f]{64}$'),
    request_hash VARCHAR(64) NOT NULL CHECK (request_hash ~ '^[0-9a-f]{64}$'),
    actor_user_id        BIGINT NOT NULL,
    audit_request_id     VARCHAR(64) NOT NULL DEFAULT '',
    reason               VARCHAR(512) NOT NULL,
    result_snapshot      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (request_id, action, idempotency_key_hash)
);

CREATE INDEX IF NOT EXISTS idx_video_admin_actions_created
    ON video_admin_actions (created_at DESC, id DESC);

CREATE TABLE IF NOT EXISTS video_ops_metrics (
    id                           BIGSERIAL PRIMARY KEY,
    bucket_at                    TIMESTAMPTZ NOT NULL,
    provider                     VARCHAR(32) NOT NULL,
    model                        VARCHAR(128) NOT NULL,
    operation                    VARCHAR(16) NOT NULL,
    group_id                     BIGINT NOT NULL,
    status_counts                JSONB NOT NULL DEFAULT '{}'::jsonb,
    submission_count             BIGINT NOT NULL DEFAULT 0,
    submission_latency_seconds   DOUBLE PRECISION,
    provider_queue_seconds       DOUBLE PRECISION,
    completion_seconds           DOUBLE PRECISION,
    rate_limit_count             BIGINT NOT NULL DEFAULT 0,
    unknown_count                BIGINT NOT NULL DEFAULT 0,
    unknown_max_age_seconds      DOUBLE PRECISION NOT NULL DEFAULT 0,
    expired_lease_count          BIGINT NOT NULL DEFAULT 0,
    pending_settlement_count     BIGINT NOT NULL DEFAULT 0,
    failed_refund_count          BIGINT NOT NULL DEFAULT 0,
    revenue                      DECIMAL(20,10) NOT NULL DEFAULT 0,
    upstream_cost                DECIMAL(20,10),
    margin                       DECIMAL(20,10),
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (bucket_at, provider, model, operation, group_id),
    CONSTRAINT video_ops_metrics_nonnegative CHECK (
        submission_count >= 0 AND rate_limit_count >= 0 AND unknown_count >= 0
        AND unknown_max_age_seconds >= 0 AND expired_lease_count >= 0
        AND pending_settlement_count >= 0 AND failed_refund_count >= 0
    )
);

CREATE INDEX IF NOT EXISTS idx_video_ops_metrics_bucket
    ON video_ops_metrics (bucket_at DESC);
