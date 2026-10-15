-- Undoes: migration 000006, the publishing state of the outbox (seq, attempts, next_attempt_at, locked_until, last_error), its indexes and the immutability trigger.
-- Loses: the retry state of unpublished events (attempts, backoff, last error, lease), the per-wallet ordering key and the protection that keeps an event's content and a published event from changing. The events themselves stay, and the unpublished ones would be published from scratch.
-- Requires: the outbox publisher stopped, so it does not fail on the missing columns.
-- Command: make migrate-down N=1
DROP TRIGGER outbox_guard ON outbox;
DROP FUNCTION outbox_guard();
DROP INDEX outbox_unpublished_by_wallet;
DROP INDEX outbox_due;
ALTER TABLE outbox DROP CONSTRAINT outbox_seq_key;
ALTER TABLE outbox
    DROP COLUMN last_error,
    DROP COLUMN locked_until,
    DROP COLUMN next_attempt_at,
    DROP COLUMN attempts,
    DROP COLUMN seq;
