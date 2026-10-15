-- Undoes: migration 000011, the columns that schedule an operation waiting for its Reference (attempts, next_attempt_at, locked_until, expires_at), their check and their two indexes.
-- Loses: when each waiting operation is next due and until when it may wait. An operation still in PENDING_REFERENCE would then be waiting with no schedule and no worker able to find it, so decide those first (let them be resolved or expire) and stop the workers.
-- Requires: the pending-reference workers stopped on every instance, and no operation left in PENDING_REFERENCE.
-- Command: make migrate-down N=1
DROP INDEX wager_transactions_unfinished;
DROP INDEX wager_transactions_pending_due;
ALTER TABLE wager_transactions DROP CONSTRAINT wager_transactions_pending_reference_check;
ALTER TABLE wager_transactions
    DROP COLUMN expires_at,
    DROP COLUMN locked_until,
    DROP COLUMN next_attempt_at,
    DROP COLUMN attempts;
