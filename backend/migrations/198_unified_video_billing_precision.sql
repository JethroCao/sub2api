-- Align the balance ledger with the ten-decimal video/subscription ledger.
-- Precision 22 preserves the legacy twelve-digit integer range of DECIMAL(20,8).

ALTER TABLE users
    ALTER COLUMN balance TYPE DECIMAL(22,10) USING balance::DECIMAL(22,10),
    ALTER COLUMN frozen_balance TYPE DECIMAL(22,10) USING frozen_balance::DECIMAL(22,10);
