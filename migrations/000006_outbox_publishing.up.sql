-- What the outbox publisher needs: an order key per wallet, retry state and a lease.
--
-- seq orders events. It is assigned at insert time, while the writing transaction
-- holds the wallet's lock, so for one wallet a later event always has a higher seq.
-- (occurred_at is the application's clock and instances do not share one.) The
-- publisher publishes a wallet's events in seq order: an event waits until every
-- earlier unpublished event of its wallet has been published.
--
-- locked_until is the lease a publisher holds while it publishes. If the publisher
-- dies, the lease runs out and another instance takes the event. All times come from
-- the database clock, so instances need not agree on time.
ALTER TABLE outbox
    ADD COLUMN seq             BIGINT GENERATED ALWAYS AS IDENTITY,
    ADD COLUMN attempts        INTEGER     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN locked_until    TIMESTAMPTZ NULL,
    ADD COLUMN last_error      TEXT        NULL;

ALTER TABLE outbox ADD CONSTRAINT outbox_seq_key UNIQUE (seq);

-- The publisher takes the oldest due events, and asks for the earlier unpublished
-- events of a wallet: both only look at unpublished rows.
CREATE INDEX outbox_due ON outbox (seq) WHERE published_at IS NULL;
CREATE INDEX outbox_unpublished_by_wallet ON outbox (aggregate_id, seq) WHERE published_at IS NULL;

-- An event is an immutable snapshot: only the publishing state may change, and a
-- published event not at all (seq is GENERATED ALWAYS, so the database already refuses
-- to update it). Without this, a bug could rewrite what was already sent.
CREATE FUNCTION outbox_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.published_at IS NOT NULL THEN
        RAISE EXCEPTION 'outbox event % was published and cannot change', OLD.event_id
            USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.event_id IS DISTINCT FROM OLD.event_id
        OR NEW.aggregate_id IS DISTINCT FROM OLD.aggregate_id
        OR NEW.event_type IS DISTINCT FROM OLD.event_type
        OR NEW.version IS DISTINCT FROM OLD.version
        OR NEW.payload IS DISTINCT FROM OLD.payload
        OR NEW.correlation_id IS DISTINCT FROM OLD.correlation_id
        OR NEW.causation_id IS DISTINCT FROM OLD.causation_id
        OR NEW.occurred_at IS DISTINCT FROM OLD.occurred_at THEN
        RAISE EXCEPTION 'the content of outbox event % never changes', OLD.event_id
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER outbox_guard
    BEFORE UPDATE ON outbox
    FOR EACH ROW EXECUTE FUNCTION outbox_guard();
