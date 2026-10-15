-- A waiting operation that cannot be tried at all (a defect, not an outage) is counted,
-- and after a few such errors it ends as FAILED instead of being tried forever. The
-- count and the last error stay on the row for whoever audits it.
ALTER TABLE wager_transactions
    ADD COLUMN error_count INTEGER NULL DEFAULT 0,
    ADD COLUMN last_error  TEXT    NULL;
UPDATE wager_transactions SET error_count = 0 WHERE error_count IS NULL;
ALTER TABLE wager_transactions
    ALTER COLUMN error_count SET NOT NULL,
    ADD CONSTRAINT wager_transactions_error_count_check CHECK (error_count >= 0),
    ADD CONSTRAINT wager_transactions_last_error_check CHECK (last_error IS NULL OR char_length(last_error) BETWEEN 1 AND 500);
