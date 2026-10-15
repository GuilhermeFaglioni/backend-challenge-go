-- A Ledger Entry has to agree with the kind of the transaction that caused it: a LOSS
-- moves no money so it has no entry, a BET only debits, and a WIN or the opening of a
-- wallet only credits. The application already works this way; the database now holds
-- it too, so that a wrong entry cannot be written whatever the application does.
-- A REFUND credits; a ROLLBACK may go either way, so it is not constrained here.
CREATE FUNCTION wallet_ledger_entries_kind_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    tx_kind TEXT;
BEGIN
    SELECT kind INTO tx_kind FROM wager_transactions WHERE id = NEW.transaction_id;
    IF tx_kind IS NULL THEN
        RETURN NEW; -- no such transaction: the foreign key reports it
    END IF;
    IF tx_kind = 'LOSS'
       OR (tx_kind = 'BET' AND NEW.direction <> 'DEBIT')
       OR (tx_kind IN ('WIN', 'OPENING', 'REFUND') AND NEW.direction <> 'CREDIT') THEN
        RAISE EXCEPTION 'a % entry does not fit a % transaction', NEW.direction, tx_kind
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER wallet_ledger_entries_kind_guard
    BEFORE INSERT ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_kind_guard();
