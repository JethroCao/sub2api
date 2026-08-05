-- Restore the shared user ledger's established scale so existing batch-image
-- holds retain their historical rounding and sufficiency behavior. Canonicalize
-- video charges to the same quantum in this forward-only correction.

-- Hold both conversion targets against concurrent writes from the preflight
-- through the ALTERs so an unsafe value cannot arrive after it was checked.
-- Take video_tasks first to match the runtime task-before-user billing order.
LOCK TABLE video_tasks, users IN ACCESS EXCLUSIVE MODE;

DO $migration_199_preflight$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM users
        WHERE balance IS NOT NULL
          AND ROUND(balance, 8) NOT BETWEEN -999999999999.99999999 AND 999999999999.99999999
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '22003',
            MESSAGE = 'migration 199 preflight failed: users.balance contains value(s) whose rounded scale-8 amount is outside DECIMAL(20,8); remediate users.balance so ROUND(value, 8) is between -999999999999.99999999 and 999999999999.99999999, then retry migration 199';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM users
        WHERE frozen_balance IS NOT NULL
          AND ROUND(frozen_balance, 8) NOT BETWEEN -999999999999.99999999 AND 999999999999.99999999
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '22003',
            MESSAGE = 'migration 199 preflight failed: users.frozen_balance contains value(s) whose rounded scale-8 amount is outside DECIMAL(20,8); remediate users.frozen_balance so ROUND(value, 8) is between -999999999999.99999999 and 999999999999.99999999, then retry migration 199';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM video_tasks
        WHERE estimated_amount IS NOT NULL
          AND ROUND(estimated_amount, 8) NOT BETWEEN -999999999999.99999999 AND 999999999999.99999999
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '22003',
            MESSAGE = 'migration 199 preflight failed: video_tasks.estimated_amount contains value(s) whose rounded scale-8 amount is outside DECIMAL(20,8); remediate video_tasks.estimated_amount so ROUND(value, 8) is between -999999999999.99999999 and 999999999999.99999999, then retry migration 199';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM video_tasks
        WHERE frozen_amount IS NOT NULL
          AND ROUND(frozen_amount, 8) NOT BETWEEN -999999999999.99999999 AND 999999999999.99999999
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '22003',
            MESSAGE = 'migration 199 preflight failed: video_tasks.frozen_amount contains value(s) whose rounded scale-8 amount is outside DECIMAL(20,8); remediate video_tasks.frozen_amount so ROUND(value, 8) is between -999999999999.99999999 and 999999999999.99999999, then retry migration 199';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM video_tasks
        WHERE settled_amount IS NOT NULL
          AND ROUND(settled_amount, 8) NOT BETWEEN -999999999999.99999999 AND 999999999999.99999999
    ) THEN
        RAISE EXCEPTION USING
            ERRCODE = '22003',
            MESSAGE = 'migration 199 preflight failed: video_tasks.settled_amount contains value(s) whose rounded scale-8 amount is outside DECIMAL(20,8); remediate video_tasks.settled_amount so ROUND(value, 8) is between -999999999999.99999999 and 999999999999.99999999, then retry migration 199';
    END IF;
END
$migration_199_preflight$;

ALTER TABLE users
    ALTER COLUMN balance TYPE DECIMAL(20,8) USING ROUND(balance, 8),
    ALTER COLUMN frozen_balance TYPE DECIMAL(20,8) USING ROUND(frozen_balance, 8);

ALTER TABLE video_tasks
    ALTER COLUMN estimated_amount TYPE DECIMAL(20,8) USING ROUND(estimated_amount, 8),
    ALTER COLUMN frozen_amount TYPE DECIMAL(20,8) USING ROUND(frozen_amount, 8),
    ALTER COLUMN settled_amount TYPE DECIMAL(20,8) USING ROUND(settled_amount, 8);
