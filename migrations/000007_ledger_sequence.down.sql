-- Undoes: migration 000007, the seq column of wallet_ledger_entries with its unique constraint and its index.
-- Loses: the order key used to list a wallet's ledger by pages. The entries themselves, and their amounts, balances and times, are untouched.
-- Requires: nothing else reverted first; the ledger listing endpoint would fail without the column, so stop the application first.
-- Command: make migrate-down N=1
DROP INDEX wallet_ledger_entries_wallet_seq;
ALTER TABLE wallet_ledger_entries DROP CONSTRAINT wallet_ledger_entries_seq_key;
ALTER TABLE wallet_ledger_entries DROP COLUMN seq;
