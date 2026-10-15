-- Undoes: migration 000010, the unique index of one processed reversal per Reference and the check that a processed reversal names its Reference.
-- Loses: the database's own barrier against a transaction being reversed twice (no row is touched). Only the application's check remains, so stop reversals before reverting and reconcile the wallets afterwards.
-- Requires: nothing else reverted first.
-- Command: make migrate-down N=1
ALTER TABLE wager_transactions DROP CONSTRAINT wager_transactions_reversal_reference_check;
DROP INDEX wager_transactions_one_reversal_per_reference;
