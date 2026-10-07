-- Request correlation for audit events (§10.1 request_id).
--
-- The server already generates a request id, logs it with every request, and hands it back to the
-- client. The audit rows did not carry it, so the step that matters in an investigation — from "the
-- user says they were denied at 14:02" or from a log line, to the exact audit events of that request
-- — required matching timestamps, which is guesswork under load.
--
-- Empty means the event was not produced by an HTTP request (a background job, or a migration), which
-- is a fact worth being able to see rather than inventing a synthetic id for.

ALTER TABLE audit.events ADD COLUMN request_id text NOT NULL DEFAULT '';
