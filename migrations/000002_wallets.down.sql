-- Undoes: migration 000002, the wallets table with its version-guard trigger and function.
-- Loses: every wallet row, that is, all balances and versions (financial data), together with the constraints and the version guard. Take a backup first if the data matters. Later migrations must be reverted before this one.
-- Requires: migrations 000003 and above already reverted (they hold foreign keys to wallets); migrate down does this in order.
-- Command: make migrate-down N=1
DROP TRIGGER wallets_version_guard ON wallets;
DROP FUNCTION wallets_version_guard();
DROP TABLE wallets;
