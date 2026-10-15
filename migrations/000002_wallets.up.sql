-- Wallets: one per player and currency, balance never negative.
--
-- The balance is changed only together with a ledger entry (migration 000004
-- adds the triggers that enforce it at commit). The version guard below makes the
-- version move only with the balance, one step at a time.
CREATE TABLE wallets (
    id                  UUID        PRIMARY KEY,
    player_id           UUID        NOT NULL,
    currency            CHAR(3)     NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),
    balance_minor       BIGINT      NOT NULL CHECK (balance_minor >= 0),
    version             BIGINT      NOT NULL CHECK (version >= 1),
    -- The Wager Transaction that produced the current balance. NULL for a wallet
    -- opened with zero that has never moved.
    last_transaction_id UUID        NULL,
    created_at          TIMESTAMPTZ NOT NULL,
    updated_at          TIMESTAMPTZ NOT NULL,
    CONSTRAINT wallets_player_currency_key UNIQUE (player_id, currency)
);

CREATE FUNCTION wallets_version_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.version <> 1 THEN
            RAISE EXCEPTION 'a wallet is created at version 1, got %', NEW.version
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF NEW.id <> OLD.id OR NEW.player_id <> OLD.player_id OR NEW.currency <> OLD.currency THEN
        RAISE EXCEPTION 'the identity of a wallet (id, player, currency) never changes'
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.balance_minor <> OLD.balance_minor THEN
        IF NEW.version <> OLD.version + 1 THEN
            RAISE EXCEPTION 'a balance change must raise the version from % to %, got %',
                OLD.version, OLD.version + 1, NEW.version USING ERRCODE = 'check_violation';
        END IF;
        IF NEW.last_transaction_id IS NULL OR NEW.last_transaction_id IS NOT DISTINCT FROM OLD.last_transaction_id THEN
            RAISE EXCEPTION 'a balance change must name a new last transaction'
                USING ERRCODE = 'check_violation';
        END IF;
    ELSIF NEW.version <> OLD.version THEN
        RAISE EXCEPTION 'the version only changes together with the balance'
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER wallets_version_guard
    BEFORE INSERT OR UPDATE ON wallets
    FOR EACH ROW EXECUTE FUNCTION wallets_version_guard();
