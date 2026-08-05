-- Idempotency keys are scoped to the authenticated caller, not to an operation.
-- Refuse to guess how historical charged tasks should be reconciled: operators
-- must resolve every duplicate set explicitly before retrying this migration.
LOCK TABLE video_tasks IN SHARE MODE;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM video_tasks
        WHERE idempotency_key_hash <> ''
        GROUP BY user_id, api_key_id, idempotency_key_hash
        HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '23505',
            MESSAGE = 'migration 200 cannot enforce video idempotency scope: duplicate non-empty idempotency keys exist for the same user_id and api_key_id across video operations; reconcile each task and its billing hold explicitly, ensure only one task retains each key, then retry migration 200';
    END IF;
END
$$;

DROP INDEX idx_video_tasks_idempotency;

CREATE UNIQUE INDEX idx_video_tasks_idempotency
    ON video_tasks (user_id, api_key_id, idempotency_key_hash)
    WHERE idempotency_key_hash <> '';
