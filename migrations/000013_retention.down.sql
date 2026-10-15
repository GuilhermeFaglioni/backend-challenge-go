-- Undoes: migration 000013, the two retention functions, the index of published events and the revoke of DELETE on the outbox.
-- Loses: no rows, but the protection that the application can delete old inbox and outbox rows only through the floors of the functions: after the revert it may delete from the outbox freely again (the inbox stays protected by migration 000008).
-- Requires: the retention worker stopped on every instance, or it fails calling the missing functions.
-- Command: make migrate-down N=1
DROP INDEX outbox_published;
GRANT DELETE, TRUNCATE ON outbox TO wagering_app;
DROP FUNCTION prune_outbox(interval, integer);
DROP FUNCTION prune_inbox(interval, integer);
