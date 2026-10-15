-- The inbox: the messages the queue consumer has handled, so that a message delivered
-- again is recognised and not handled twice. A row is written in the same database
-- transaction as the changes the message caused, so it exists exactly when they do.
CREATE TABLE inbox (
    consumer_name TEXT        NOT NULL CHECK (consumer_name <> ''),
    message_id    TEXT        NOT NULL CHECK (message_id <> '' AND char_length(message_id) <= 128),
    -- SHA-256 in lower-case hex of what the message asked for. The same message id with
    -- another hash is a different message under a reused id, not a redelivery.
    payload_hash  TEXT        NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    received_at   TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ NULL,
    PRIMARY KEY (consumer_name, message_id),
    CHECK (completed_at IS NULL OR completed_at >= received_at)
);

-- The application only adds rows and completes them: it can neither change what a row
-- says about the message nor forget a message.
REVOKE UPDATE, DELETE, TRUNCATE ON inbox FROM wagering_app;
GRANT UPDATE (completed_at) ON inbox TO wagering_app;
