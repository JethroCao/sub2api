ALTER TABLE user_subscriptions
    ADD COLUMN IF NOT EXISTS frozen_quota DECIMAL(20,10) NOT NULL DEFAULT 0;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'user_subscriptions_frozen_quota_non_negative'
          AND conrelid = 'user_subscriptions'::regclass
    ) THEN
        ALTER TABLE user_subscriptions
            ADD CONSTRAINT user_subscriptions_frozen_quota_non_negative
            CHECK (frozen_quota >= 0);
    END IF;
END;
$$;

COMMENT ON COLUMN user_subscriptions.frozen_quota IS
    'Video quota reserved but not yet captured or released';
