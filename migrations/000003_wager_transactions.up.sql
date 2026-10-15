-- Wager Transactions: the identity of what was asked and its outcome.
--
-- One table for external Operation requests and for the internal opening of a
-- funded wallet, told apart by origin. Retry and lease columns (attempts,
-- next_attempt_at, locked_until, expires_at) and the reversal index belong to the
-- tickets that use them.
CREATE TABLE wager_transactions (
    id                                UUID        PRIMARY KEY,
    origin                            TEXT        NOT NULL CHECK (origin IN ('EXTERNAL', 'INTERNAL')),
    kind                              TEXT        NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),

    provider_id                       TEXT        NULL,
    external_transaction_id           TEXT        NULL,
    idempotency_key                   TEXT        NULL,
    payload_hash                      TEXT        NULL,

    -- The wallet id as received, with no foreign key; wallet_id is the real one.
    requested_wallet_id               UUID        NOT NULL,
    wallet_id                         UUID        NULL REFERENCES wallets (id),
    player_id                         UUID        NOT NULL,
    round_id                          TEXT        NULL,
    game_id                           TEXT        NULL,
    amount_minor                      BIGINT      NOT NULL,
    currency                          CHAR(3)     NOT NULL CHECK (currency IN ('BRL', 'USD', 'EUR')),

    reference_external_transaction_id TEXT        NULL,
    reference_transaction_id          UUID        NULL REFERENCES wager_transactions (id),

    status                            TEXT        NOT NULL CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),
    failure_code                      TEXT        NULL,
    result_balance_minor              BIGINT      NULL CHECK (result_balance_minor >= 0),
    result_wallet_version             BIGINT      NULL CHECK (result_wallet_version >= 1),

    correlation_id                    TEXT        NULL,
    created_at                        TIMESTAMPTZ NOT NULL,
    updated_at                        TIMESTAMPTZ NOT NULL,
    processed_at                      TIMESTAMPTZ NULL,

    -- An internal opening and an external request cannot be mistaken for each other,
    -- even if the application got it wrong.
    CONSTRAINT wager_transactions_origin_fields_check CHECK (
        (origin = 'INTERNAL' AND kind = 'OPENING'
            AND provider_id IS NULL AND external_transaction_id IS NULL AND idempotency_key IS NULL
            AND payload_hash IS NULL AND round_id IS NULL AND game_id IS NULL
            AND reference_external_transaction_id IS NULL AND reference_transaction_id IS NULL)
        OR
        (origin = 'EXTERNAL' AND kind <> 'OPENING'
            AND provider_id IS NOT NULL AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL AND payload_hash IS NOT NULL)
    ),
    -- Only the rejection for a wallet that does not exist may have no wallet.
    CONSTRAINT wager_transactions_wallet_check CHECK (
        wallet_id IS NOT NULL OR (status = 'REJECTED' AND failure_code = 'WALLET_NOT_FOUND')
    ),
    CONSTRAINT wager_transactions_amount_check CHECK (
        (kind = 'LOSS' AND amount_minor = 0) OR (kind <> 'LOSS' AND amount_minor > 0)
    ),
    -- Only the two endings that explain themselves carry a failure code.
    CONSTRAINT wager_transactions_failure_code_check CHECK (
        (status IN ('REJECTED', 'FAILED')) = (failure_code IS NOT NULL)
    )
);

-- The idempotency key is scoped by provider: one provider's key never collides with
-- another's, and a collision reveals nothing about someone else's transactions.
CREATE UNIQUE INDEX wager_transactions_idempotency_key
    ON wager_transactions (provider_id, idempotency_key) WHERE origin = 'EXTERNAL';
CREATE UNIQUE INDEX wager_transactions_external_id_key
    ON wager_transactions (provider_id, external_transaction_id) WHERE origin = 'EXTERNAL';
-- One opening credit per wallet.
CREATE UNIQUE INDEX wager_transactions_one_opening_per_wallet
    ON wager_transactions (wallet_id) WHERE kind = 'OPENING';

-- A finished transaction never changes, and what identifies a request and its money
-- never changes in any state. This is the second barrier behind the domain's state
-- machine.
CREATE FUNCTION wager_transactions_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status IN ('PROCESSED', 'REJECTED', 'FAILED') THEN
        RAISE EXCEPTION 'wager transaction % is % and cannot change', OLD.id, OLD.status
            USING ERRCODE = 'check_violation';
    END IF;

    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.origin IS DISTINCT FROM OLD.origin
        OR NEW.kind IS DISTINCT FROM OLD.kind
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.external_transaction_id IS DISTINCT FROM OLD.external_transaction_id
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
        OR NEW.requested_wallet_id IS DISTINCT FROM OLD.requested_wallet_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.player_id IS DISTINCT FROM OLD.player_id
        OR NEW.amount_minor IS DISTINCT FROM OLD.amount_minor
        OR NEW.currency IS DISTINCT FROM OLD.currency THEN
        RAISE EXCEPTION 'the identity and the money of wager transaction % never change', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;

    -- PENDING may go anywhere; PENDING_REFERENCE only forward; nothing goes back.
    IF OLD.status = 'PENDING_REFERENCE' AND NEW.status = 'PENDING' THEN
        RAISE EXCEPTION 'wager transaction % cannot go back to PENDING', OLD.id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER wager_transactions_guard
    BEFORE UPDATE ON wager_transactions
    FOR EACH ROW EXECUTE FUNCTION wager_transactions_guard();
