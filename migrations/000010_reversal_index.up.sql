-- One reversal per Reference. A transaction that was reversed (a REFUND or a ROLLBACK
-- that was processed) cannot be reversed again, whichever of the two comes second, and
-- whatever the application does or fails to check.
CREATE UNIQUE INDEX wager_transactions_one_reversal_per_reference
    ON wager_transactions (reference_transaction_id)
    WHERE kind IN ('REFUND', 'ROLLBACK') AND status = 'PROCESSED';

-- The index cannot see a reversal that forgot its Reference (NULLs are all different),
-- so a processed reversal must name the transaction it reverses.
ALTER TABLE wager_transactions ADD CONSTRAINT wager_transactions_reversal_reference_check CHECK (
    NOT (kind IN ('REFUND', 'ROLLBACK') AND status = 'PROCESSED') OR reference_transaction_id IS NOT NULL
);
