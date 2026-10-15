-- The outbox: domain events written in the same database transaction as the change
-- that caused them, published later by a separate process. The retry and lease
-- columns belong to the ticket that adds the publisher.
CREATE TABLE outbox (
    event_id       UUID        PRIMARY KEY,
    aggregate_id   UUID        NOT NULL,
    event_type     TEXT        NOT NULL CHECK (event_type <> ''),
    version        INTEGER     NOT NULL CHECK (version >= 1),
    -- An immutable snapshot of the event, an object by contract.
    payload        JSONB       NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    correlation_id TEXT        NULL,
    causation_id   TEXT        NULL,
    occurred_at    TIMESTAMPTZ NOT NULL,
    published_at   TIMESTAMPTZ NULL
);

-- What the publisher scans: only the events not yet published, oldest first.
CREATE INDEX outbox_unpublished ON outbox (occurred_at) WHERE published_at IS NULL;
