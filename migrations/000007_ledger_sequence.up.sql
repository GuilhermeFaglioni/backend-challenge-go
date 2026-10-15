-- The order of a wallet's ledger, for listing it page by page.
--
-- seq is assigned by the database when the entry is inserted. Entries of one wallet are
-- inserted while their transaction holds that wallet's lock, which it keeps until it
-- commits, so for one wallet a later entry always has a higher seq AND commits after the
-- earlier one: a reader that has seen up to some seq never finds an earlier one
-- appearing later. created_at cannot promise this, because it is the application's clock
-- and instances do not share one.
ALTER TABLE wallet_ledger_entries
    ADD COLUMN seq BIGINT GENERATED ALWAYS AS IDENTITY;

ALTER TABLE wallet_ledger_entries ADD CONSTRAINT wallet_ledger_entries_seq_key UNIQUE (seq);

-- A page of a wallet's ledger: from a seq on, in order.
CREATE INDEX wallet_ledger_entries_wallet_seq ON wallet_ledger_entries (wallet_id, seq);
