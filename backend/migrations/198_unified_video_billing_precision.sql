-- Canonicalize video charges to the existing user-ledger quantum without
-- changing batch-image balance behavior on the shared DECIMAL(20,8) ledger.

ALTER TABLE video_tasks
    ALTER COLUMN estimated_amount TYPE DECIMAL(20,8) USING ROUND(estimated_amount, 8),
    ALTER COLUMN frozen_amount TYPE DECIMAL(20,8) USING ROUND(frozen_amount, 8),
    ALTER COLUMN settled_amount TYPE DECIMAL(20,8) USING ROUND(settled_amount, 8);
