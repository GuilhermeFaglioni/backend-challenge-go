-- Undoes: migration 000005, the outbox table and its index for unpublished events.
-- Loses: every event in the outbox, including those not yet published, which are then never sent to the queue (consumers miss them); the published ones are only history. Take a backup first, and stop the publisher before reverting.
-- Requires: the outbox publisher stopped, so it does not fail on the missing table.
-- Command: make migrate-down N=1
DROP INDEX outbox_unpublished;
DROP TABLE outbox;
