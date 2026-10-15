-- Undoes: migration 000012, the error count and the last error of operations that could not be tried.
-- Loses: how many times each operation failed to be tried, and why it last failed (the operations that ended as FAILED stay FAILED, with their failure code).
-- Requires: the pending-reference workers stopped on every instance.
-- Command: make migrate-down N=1
ALTER TABLE wager_transactions
    DROP CONSTRAINT wager_transactions_last_error_check,
    DROP CONSTRAINT wager_transactions_error_count_check,
    DROP COLUMN last_error,
    DROP COLUMN error_count;
