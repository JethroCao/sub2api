-- PostgreSQL is the source of truth for public video task ownership,
-- idempotency, state, polling leases, price snapshots, and events.

CREATE TABLE IF NOT EXISTS video_tasks (
    id                        BIGSERIAL PRIMARY KEY,
    request_id                VARCHAR(36) NOT NULL UNIQUE
                              CHECK (request_id ~ '^vid_[0-9a-f]{32}$'),
    user_id                   BIGINT NOT NULL,
    api_key_id                BIGINT NOT NULL,
    subscription_id           BIGINT,
    group_id                  BIGINT NOT NULL,
    account_id                BIGINT NOT NULL,
    platform                  VARCHAR(16) NOT NULL CHECK (platform = 'video'),
    provider                  VARCHAR(32) NOT NULL CHECK (provider IN ('seedance', 'kling')),
    operation                 VARCHAR(16) NOT NULL CHECK (operation IN ('generation', 'edit', 'extension')),
    external_model            VARCHAR(128) NOT NULL,
    upstream_model            VARCHAR(128) NOT NULL,
    idempotency_key_hash      VARCHAR(64) NOT NULL DEFAULT '',
    request_hash              VARCHAR(64) NOT NULL,
    provider_submission_token VARCHAR(128),
    request_payload           JSONB,
    status                    VARCHAR(16) NOT NULL DEFAULT 'created'
                              CHECK (status IN ('created', 'submitting', 'submitted', 'queued', 'running', 'succeeded', 'failed', 'cancelled', 'unknown')),
    upstream_task_id          VARCHAR(255),
    upstream_status           VARCHAR(64),
    result_url                TEXT,
    result_url_expires_at     TIMESTAMPTZ,
    result_content_type       VARCHAR(128),
    result_duration_seconds   DECIMAL(20,6),
    result_width              INTEGER,
    result_height             INTEGER,
    pricing_unit              VARCHAR(32) NOT NULL,
    unit_price                DECIMAL(20,10) NOT NULL CHECK (unit_price >= 0),
    estimated_units           DECIMAL(20,6) NOT NULL CHECK (estimated_units >= 0),
    estimated_amount          DECIMAL(20,10) NOT NULL CHECK (estimated_amount >= 0),
    frozen_amount             DECIMAL(20,10) NOT NULL CHECK (frozen_amount >= 0),
    settled_amount            DECIMAL(20,10),
    currency                  VARCHAR(16) NOT NULL DEFAULT 'USD',
    billing_mode              VARCHAR(32) NOT NULL,
    billing_status            VARCHAR(32) NOT NULL,
    billing_reference         VARCHAR(128),
    submission_attempts       INTEGER NOT NULL DEFAULT 0 CHECK (submission_attempts >= 0),
    poll_attempts             INTEGER NOT NULL DEFAULT 0 CHECK (poll_attempts >= 0),
    settlement_attempts       INTEGER NOT NULL DEFAULT 0 CHECK (settlement_attempts >= 0),
    next_poll_at              TIMESTAMPTZ,
    lease_owner               VARCHAR(128),
    lease_expires_at          TIMESTAMPTZ,
    last_error_code           VARCHAR(128),
    last_error_message        TEXT,
    last_error_retryable      BOOLEAN NOT NULL DEFAULT FALSE,
    version                   BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    submitted_at              TIMESTAMPTZ,
    started_at                TIMESTAMPTZ,
    finished_at               TIMESTAMPTZ,
    settled_at                TIMESTAMPTZ,
    CHECK (settled_amount IS NULL OR (settled_amount >= 0 AND settled_amount <= frozen_amount)),
    CHECK (upstream_task_id IS NULL OR upstream_task_id <> request_id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_video_tasks_idempotency
    ON video_tasks (user_id, api_key_id, operation, idempotency_key_hash)
    WHERE idempotency_key_hash <> '';

CREATE INDEX IF NOT EXISTS idx_video_tasks_status_next_poll
    ON video_tasks (status, next_poll_at);

CREATE INDEX IF NOT EXISTS idx_video_tasks_lease_expires
    ON video_tasks (lease_expires_at);

CREATE INDEX IF NOT EXISTS idx_video_tasks_upstream_identity
    ON video_tasks (provider, account_id, upstream_task_id)
    WHERE upstream_task_id IS NOT NULL AND upstream_task_id <> '';

CREATE INDEX IF NOT EXISTS idx_video_tasks_owner
    ON video_tasks (user_id, api_key_id, created_at DESC);

CREATE TABLE IF NOT EXISTS video_task_events (
    id          BIGSERIAL PRIMARY KEY,
    request_id  VARCHAR(36) NOT NULL REFERENCES video_tasks(request_id) ON DELETE CASCADE,
    event_type  VARCHAR(64) NOT NULL,
    payload     JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_video_task_events_request_created
    ON video_task_events (request_id, created_at, id);

CREATE INDEX IF NOT EXISTS idx_video_task_events_type_created
    ON video_task_events (event_type, created_at);

CREATE OR REPLACE FUNCTION reject_video_task_event_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'video task events are append-only' USING ERRCODE = '55000';
END;
$$ LANGUAGE plpgsql;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgname = 'trg_video_task_events_append_only'
          AND tgrelid = 'video_task_events'::regclass
    ) THEN
        CREATE TRIGGER trg_video_task_events_append_only
        BEFORE UPDATE OR DELETE ON video_task_events
        FOR EACH ROW EXECUTE FUNCTION reject_video_task_event_mutation();
    END IF;
END;
$$;

COMMENT ON COLUMN video_tasks.request_payload IS
    'Bounded minimized JSON only; cleared atomically when upstream_task_id is stored';
COMMENT ON COLUMN video_tasks.unit_price IS
    'Final customer unit price snapshotted at task creation';
COMMENT ON COLUMN video_tasks.frozen_amount IS
    'Maximum customer charge frozen for this task; settlement cannot exceed it';
