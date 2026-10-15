-- Undoes: migration 000008, the inbox table.
-- Loses: the record of every message the consumer has handled. Redelivered messages are then no longer recognised as handled by the inbox; the financial idempotency key still stops a second movement, but a message that was refused would be handled again.
-- Requires: the SQS consumer stopped on every instance, so it does not fail on the missing table.
-- Command: make migrate-down N=1
DROP TABLE inbox;
