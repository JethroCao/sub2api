-- New Grok video submissions join the durable video ledger while the legacy
-- Redis binding remains read-only compatibility state for pre-deployment IDs.
ALTER TABLE video_tasks
    DROP CONSTRAINT video_tasks_platform_check,
    DROP CONSTRAINT video_tasks_provider_check;

ALTER TABLE video_tasks
    ADD CONSTRAINT video_tasks_platform_check
        CHECK (platform IN ('video', 'grok')),
    ADD CONSTRAINT video_tasks_provider_check
        CHECK (provider IN ('seedance', 'kling', 'grok')),
    ADD CONSTRAINT video_tasks_platform_provider_check
        CHECK (
            (platform = 'grok' AND provider = 'grok')
            OR (platform = 'video' AND provider IN ('seedance', 'kling'))
        );
