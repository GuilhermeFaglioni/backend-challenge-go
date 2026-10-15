-- The wallet ledger: append-only, with the arithmetic checked per row and the
-- coherence with the wallet's balance checked at commit.
CREATE TABLE wallet_ledger_entries (
    id                   UUID        PRIMARY KEY,
    wallet_id            UUID        NOT NULL REFERENCES wallets (id),
    transaction_id       UUID        NOT NULL REFERENCES wager_transactions (id),
    direction            TEXT        NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    amount_minor         BIGINT      NOT NULL CHECK (amount_minor > 0),
    balance_before_minor BIGINT      NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor  BIGINT      NOT NULL CHECK (balance_after_minor >= 0),
    created_at           TIMESTAMPTZ NOT NULL,
    CONSTRAINT wallet_ledger_entries_wallet_transaction_key UNIQUE (wallet_id, transaction_id),
    CONSTRAINT wallet_ledger_entries_arithmetic_check CHECK (
        (direction = 'CREDIT' AND balance_after_minor = balance_before_minor + amount_minor)
        OR (direction = 'DEBIT' AND balance_after_minor = balance_before_minor - amount_minor)
    )
);

-- Append-only: no edits, no deletes, no truncate, for everyone. The trigger is the
-- barrier for the table owner; the revoke below keeps the application role out
-- altogether.
CREATE FUNCTION wallet_ledger_entries_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the ledger is append-only: % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;

CREATE TRIGGER wallet_ledger_entries_no_edit
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_append_only();
CREATE TRIGGER wallet_ledger_entries_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION wallet_ledger_entries_append_only();

REVOKE UPDATE, DELETE, TRUNCATE ON wallet_ledger_entries FROM wagering_app;

-- A balance change needs its entry. Checked at commit, because the wallet and the
-- entry are written in the same database transaction in either order. The entry must
-- start from the balance the wallet had and end at the one it has now.
CREATE FUNCTION wallets_ledger_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    previous BIGINT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        previous := 0;
    ELSE
        previous := OLD.balance_minor;
    END IF;
    IF NEW.balance_minor = previous THEN
        RETURN NULL; -- opened empty, or touched without a balance change
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM wallet_ledger_entries e
        WHERE e.wallet_id = NEW.id
          AND e.transaction_id = NEW.last_transaction_id
          AND e.balance_before_minor = previous
          AND e.balance_after_minor = NEW.balance_minor
    ) THEN
        RAISE EXCEPTION 'wallet % changed balance from % to % without a matching ledger entry',
            NEW.id, previous, NEW.balance_minor USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER wallets_ledger_guard
    AFTER INSERT OR UPDATE OF balance_minor ON wallets
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallets_ledger_guard();

-- And the other way round: an entry needs the balance change it describes.
CREATE FUNCTION wallet_ledger_entries_wallet_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM wallets w
        WHERE w.id = NEW.wallet_id
          AND w.last_transaction_id = NEW.transaction_id
          AND w.balance_minor = NEW.balance_after_minor
    ) THEN
        RAISE EXCEPTION 'ledger entry % does not match the balance and last transaction of wallet %',
            NEW.id, NEW.wallet_id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER wallet_ledger_entries_wallet_guard
    AFTER INSERT ON wallet_ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_wallet_guard();
