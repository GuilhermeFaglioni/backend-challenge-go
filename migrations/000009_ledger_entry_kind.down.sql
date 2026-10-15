-- Undoes: migration 000009, the trigger that checks a Ledger Entry against the kind of its transaction.
-- Loses: only the check; no data. Entries already written stay as they are.
-- Requires: nothing else reverted first; until the check is back, the database no longer refuses an entry of the wrong direction or one for a LOSS.
-- Command: make migrate-down N=1
DROP TRIGGER wallet_ledger_entries_kind_guard ON wallet_ledger_entries;
DROP FUNCTION wallet_ledger_entries_kind_guard();
