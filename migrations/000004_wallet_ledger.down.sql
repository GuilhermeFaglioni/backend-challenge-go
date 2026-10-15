-- Undoes: migration 000004, the wallet_ledger_entries table with its append-only and coherence triggers, functions and constraints.
-- Loses: THE WHOLE FINANCIAL HISTORY. Every ledger entry is dropped, so balances can no longer be explained or reconciled, and the protections that keep the ledger append-only and in step with the wallets are gone. Take a backup first. This is a destructive rollback meant for development; do not run it on data that matters.
-- Requires: migrations 000005 and above already reverted; migrate down does this in order.
-- Command: make migrate-down N=1
DROP TRIGGER wallet_ledger_entries_wallet_guard ON wallet_ledger_entries;
DROP FUNCTION wallet_ledger_entries_wallet_guard();
DROP TRIGGER wallets_ledger_guard ON wallets;
DROP FUNCTION wallets_ledger_guard();
DROP TRIGGER wallet_ledger_entries_no_truncate ON wallet_ledger_entries;
DROP TRIGGER wallet_ledger_entries_no_edit ON wallet_ledger_entries;
DROP FUNCTION wallet_ledger_entries_append_only();
DROP TABLE wallet_ledger_entries;
