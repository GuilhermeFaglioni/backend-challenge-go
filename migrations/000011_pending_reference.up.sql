-- An operation whose Reference has not arrived waits for it in the database: how many
-- times it was tried, when the next try is due, until when it may wait, and which worker
-- holds it right now. Surviving a restart or the death of an instance is the point.
ALTER TABLE wager_transactions
    ADD COLUMN attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN next_attempt_at TIMESTAMPTZ NULL,
    ADD COLUMN locked_until    TIMESTAMPTZ NULL,
    ADD COLUMN expires_at      TIMESTAMPTZ NULL;

-- Waiting without a schedule would be waiting forever: no worker would ever come for it.
ALTER TABLE wager_transactions ADD CONSTRAINT wager_transactions_pending_reference_check CHECK (
    status <> 'PENDING_REFERENCE'
    OR (reference_external_transaction_id IS NOT NULL
        AND attempts >= 1 AND next_attempt_at IS NOT NULL AND expires_at IS NOT NULL AND expires_at > created_at)
);

-- What a worker scans: the operations whose next try has come, and the ones that were
-- accepted and never finished.
CREATE INDEX wager_transactions_pending_due
    ON wager_transactions (next_attempt_at) WHERE status = 'PENDING_REFERENCE';
CREATE INDEX wager_transactions_unfinished
    ON wager_transactions (created_at) WHERE status = 'PENDING';
