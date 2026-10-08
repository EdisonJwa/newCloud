-- Auth method on audit events (§10.1 auth_method).
--
-- The audit log recorded WHO acted and from WHERE, and not HOW they authenticated. That is the field
-- an investigation reaches for first: a stolen session cookie and a leaked app password look
-- identical in every other column, and the response is different — revoking an app password is
-- surgical, invalidating every session is not.
--
-- Empty means the event was written outside any authentication (a background job), which is a fact
-- worth preserving rather than filling with a guess.

ALTER TABLE audit.events ADD COLUMN auth_method text NOT NULL DEFAULT '';
