-- Undoes: migration 000014, the reconciliation checkpoints table and the function that records them.
-- Loses: every recorded checkpoint; the ledger is untouched, and a reconciliation after the revert reads the whole ledger again, as before.
-- Requires: the application of the previous version on every instance, or reconciliations fail reading the missing table.
-- Command: make migrate-down N=1
DROP FUNCTION record_reconciliation_checkpoint(uuid, bigint);
DROP TABLE reconciliation_checkpoints;
DROP FUNCTION reconciliation_checkpoints_append_only();
