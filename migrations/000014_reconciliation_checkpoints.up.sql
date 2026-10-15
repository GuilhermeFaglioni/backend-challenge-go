-- Reconciliation checkpoints: what a wallet's ledger adds up to through one seq, so that
-- a reconciliation reads only the entries after the last checkpoint instead of the whole
-- ledger.
--
-- A checkpoint is trusted only because of how it is made. The application role cannot
-- write the table: the only way in is record_reconciliation_checkpoint, which runs as the
-- owner and computes the sums itself from the ledger, continuing from the previous
-- checkpoint, and refuses to record them unless they add up to the balance_after_minor of
-- the entry at that seq. The ledger is append-only (migration 000004) and, for one wallet,
-- a later entry always has a higher seq and commits later (migration 000007), so the
-- entries through a recorded seq can no longer change. A full reconciliation re-reads the
-- whole ledger and checks the last checkpoint against it.
CREATE TABLE reconciliation_checkpoints (
    wallet_id     UUID        NOT NULL REFERENCES wallets (id),
    through_seq   BIGINT      NOT NULL CHECK (through_seq >= 1),
    credits_minor NUMERIC     NOT NULL CHECK (credits_minor >= 0),
    debits_minor  NUMERIC     NOT NULL CHECK (debits_minor >= 0),
    entries       BIGINT      NOT NULL CHECK (entries >= 1),
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (wallet_id, through_seq)
);

-- Read by the application, written only through the function below, never changed.
REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON reconciliation_checkpoints FROM wagering_app;

CREATE FUNCTION reconciliation_checkpoints_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'reconciliation checkpoints are append-only: % is not allowed', TG_OP
        USING ERRCODE = 'restrict_violation';
END
$$;

CREATE TRIGGER reconciliation_checkpoints_no_edit
    BEFORE UPDATE OR DELETE ON reconciliation_checkpoints
    FOR EACH ROW EXECUTE FUNCTION reconciliation_checkpoints_append_only();
CREATE TRIGGER reconciliation_checkpoints_no_truncate
    BEFORE TRUNCATE ON reconciliation_checkpoints
    FOR EACH STATEMENT EXECUTE FUNCTION reconciliation_checkpoints_append_only();

-- Records the checkpoint of the wallet through the entry at through_seq. Returns false
-- when a checkpoint at or beyond that seq is already there (another reconciliation got
-- there first), and fails when the seq is not an entry of the wallet or the ledger does
-- not add up.
CREATE FUNCTION record_reconciliation_checkpoint(p_wallet UUID, p_through BIGINT) RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    base_seq     BIGINT  := 0;
    base_credits NUMERIC := 0;
    base_debits  NUMERIC := 0;
    base_entries BIGINT  := 0;
    last_after   BIGINT;
    credits      NUMERIC;
    debits       NUMERIC;
    counted      BIGINT;
BEGIN
    SELECT through_seq, credits_minor, debits_minor, entries
      INTO base_seq, base_credits, base_debits, base_entries
      FROM reconciliation_checkpoints
     WHERE wallet_id = p_wallet
     ORDER BY through_seq DESC
     LIMIT 1;
    IF NOT FOUND THEN
        base_seq := 0; base_credits := 0; base_debits := 0; base_entries := 0;
    END IF;
    IF base_seq >= p_through THEN
        RETURN false;
    END IF;

    SELECT balance_after_minor INTO last_after
      FROM wallet_ledger_entries
     WHERE wallet_id = p_wallet AND seq = p_through;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'seq % is not an entry of wallet %', p_through, p_wallet
            USING ERRCODE = 'check_violation';
    END IF;

    SELECT coalesce(sum(amount_minor) FILTER (WHERE direction = 'CREDIT'), 0),
           coalesce(sum(amount_minor) FILTER (WHERE direction = 'DEBIT'), 0),
           count(*)
      INTO credits, debits, counted
      FROM wallet_ledger_entries
     WHERE wallet_id = p_wallet AND seq > base_seq AND seq <= p_through;

    credits := credits + base_credits;
    debits  := debits + base_debits;
    IF credits - debits <> last_after THEN
        RAISE EXCEPTION 'the ledger of wallet % adds up to % through seq %, but that entry ends at %',
            p_wallet, credits - debits, p_through, last_after
            USING ERRCODE = 'check_violation';
    END IF;

    INSERT INTO reconciliation_checkpoints (wallet_id, through_seq, credits_minor, debits_minor, entries)
    VALUES (p_wallet, p_through, credits, debits, base_entries + counted)
    ON CONFLICT DO NOTHING;
    RETURN FOUND;
END
$$;

REVOKE ALL ON FUNCTION record_reconciliation_checkpoint(uuid, bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION record_reconciliation_checkpoint(uuid, bigint) TO wagering_app;
