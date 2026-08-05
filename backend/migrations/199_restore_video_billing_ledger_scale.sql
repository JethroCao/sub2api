-- Restore the shared user ledger's established scale so existing batch-image
-- holds retain their historical rounding and sufficiency behavior. Canonicalize
-- video charges to the same quantum in this forward-only correction.

ALTER TABLE users
    ALTER COLUMN balance TYPE DECIMAL(20,8) USING ROUND(balance, 8),
    ALTER COLUMN frozen_balance TYPE DECIMAL(20,8) USING ROUND(frozen_balance, 8);

ALTER TABLE video_tasks
    ALTER COLUMN estimated_amount TYPE DECIMAL(20,8) USING ROUND(estimated_amount, 8),
    ALTER COLUMN frozen_amount TYPE DECIMAL(20,8) USING ROUND(frozen_amount, 8),
    ALTER COLUMN settled_amount TYPE DECIMAL(20,8) USING ROUND(settled_amount, 8);
