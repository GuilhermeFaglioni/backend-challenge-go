-- Retention of the inbox and the outbox. Both only grow: a row per handled message and a
-- row per event. The application may now remove the old ones, but only through these two
-- functions, which run as the owner and refuse anything but rows that have done their job
-- long enough ago: the role itself cannot delete from either table.
--
-- The floors are not arbitrary:
--   * a message can be delivered again only while the input queue keeps it (four days, the
--     SQS default the queue uses); the inbox must remember a message longer than that, or
--     its redelivery would be handled again. The floor is seven days.
--   * a published event is no longer needed by the publisher at all; one day is kept for
--     whoever investigates a delivery.

CREATE FUNCTION prune_inbox(older_than interval, max_rows integer) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    removed integer;
BEGIN
    IF older_than < interval '7 days' THEN
        RAISE EXCEPTION 'the inbox remembers a message for at least 7 days, longer than the queue can deliver it again'
            USING ERRCODE = 'check_violation';
    END IF;
    IF max_rows < 1 THEN
        RAISE EXCEPTION 'max_rows must be positive' USING ERRCODE = 'check_violation';
    END IF;
    DELETE FROM inbox
    WHERE (consumer_name, message_id) IN (
        SELECT consumer_name, message_id FROM inbox
        WHERE completed_at IS NOT NULL AND completed_at < now() - older_than
        LIMIT max_rows);
    GET DIAGNOSTICS removed = ROW_COUNT;
    RETURN removed;
END
$$;

CREATE FUNCTION prune_outbox(older_than interval, max_rows integer) RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    removed integer;
BEGIN
    IF older_than < interval '1 day' THEN
        RAISE EXCEPTION 'a published event is kept for at least 1 day' USING ERRCODE = 'check_violation';
    END IF;
    IF max_rows < 1 THEN
        RAISE EXCEPTION 'max_rows must be positive' USING ERRCODE = 'check_violation';
    END IF;
    DELETE FROM outbox
    WHERE event_id IN (
        SELECT event_id FROM outbox
        WHERE published_at IS NOT NULL AND published_at < now() - older_than
        LIMIT max_rows);
    GET DIAGNOSTICS removed = ROW_COUNT;
    RETURN removed;
END
$$;

REVOKE ALL ON FUNCTION prune_inbox(interval, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION prune_outbox(interval, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION prune_inbox(interval, integer) TO wagering_app;
GRANT EXECUTE ON FUNCTION prune_outbox(interval, integer) TO wagering_app;

-- The role deletes from the outbox only through the function now.
REVOKE DELETE, TRUNCATE ON outbox FROM wagering_app;

-- What the pruning scans: the published events by when they were published.
CREATE INDEX outbox_published ON outbox (published_at) WHERE published_at IS NOT NULL;
