-- Fixed, cumulative latency histograms are mergeable across bounded metric
-- dimensions and time buckets. This is a forward-only extension of migration 203.

ALTER TABLE video_ops_metrics
    ADD COLUMN IF NOT EXISTS submission_latency_histogram JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS provider_queue_histogram JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS completion_histogram JSONB NOT NULL DEFAULT '{}'::jsonb;
