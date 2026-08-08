-- Accepted Kling tasks need one non-sensitive discriminator to recover the
-- provider-specific query route after a restart. Keep the original privacy
-- invariant for every other provider and reject any additional JSON field.
ALTER TABLE video_tasks
    DROP CONSTRAINT video_tasks_upstream_payload_cleared;

ALTER TABLE video_tasks
    ADD CONSTRAINT video_tasks_upstream_payload_cleared CHECK (
        upstream_task_id IS NULL
        OR request_payload IS NULL
        OR (
            provider = 'kling'
            AND request_payload = jsonb_build_object(
                'provider_task_kind',
                request_payload ->> 'provider_task_kind'
            )
            AND (
                (operation = 'generation' AND request_payload ->> 'provider_task_kind' IN ('text2video', 'image2video'))
                OR (operation = 'extension' AND request_payload ->> 'provider_task_kind' = 'video-extend')
            )
        )
    );

COMMENT ON COLUMN video_tasks.request_payload IS
    'While submitting, stores the minimized recovery snapshot; accepted Kling tasks store only provider_task_kind; other accepted tasks store NULL.';
