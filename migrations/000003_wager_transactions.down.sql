-- Undoes: migration 000003, the wager_transactions table with its indexes, constraints, guard trigger and function.
-- Loses: every Wager Transaction (the record of what providers asked and how it ended, including idempotency keys), so replays of past requests stop being recognised; also the uniqueness and immutability protections. Take a backup first if the data matters.
-- Requires: migrations 000004 and above already reverted (the ledger and the outbox refer to it); migrate down does this in order.
-- Command: make migrate-down N=1
DROP TRIGGER wager_transactions_guard ON wager_transactions;
DROP FUNCTION wager_transactions_guard();
DROP TABLE wager_transactions;
