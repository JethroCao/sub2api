-- Durable public-video authorization and group-scoped customer pricing.
-- unit_price is the final customer price and is never multiplied by the
-- group's token rate multiplier. upstream_unit_cost is optional ops data.

ALTER TABLE groups
    ADD COLUMN IF NOT EXISTS allow_video_generation BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS video_pricing_rules (
    id                 BIGSERIAL PRIMARY KEY,
    group_id           BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    external_model     VARCHAR(128) NOT NULL,
    operation          VARCHAR(16) NOT NULL CHECK (operation IN ('generation', 'edit', 'extension')),
    resolution         VARCHAR(32) NOT NULL DEFAULT '*',
    audio_mode         VARCHAR(16) NOT NULL DEFAULT 'any' CHECK (audio_mode IN ('any', 'with_audio', 'without_audio')),
    unit               VARCHAR(32) NOT NULL CHECK (unit IN ('per_request', 'per_output_second')),
    unit_price         DECIMAL(20,10) NOT NULL,
    upstream_unit_cost DECIMAL(20,10),
    enabled            BOOLEAN NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (group_id, external_model, operation, resolution, audio_mode)
);

CREATE INDEX IF NOT EXISTS idx_video_pricing_rules_lookup
    ON video_pricing_rules (group_id, external_model, operation, enabled);

COMMENT ON COLUMN video_pricing_rules.unit_price IS
    'Final customer price; never multiplied by the group token rate multiplier';
COMMENT ON COLUMN video_pricing_rules.upstream_unit_cost IS
    'Optional upstream cost for margin and operations reporting only';
