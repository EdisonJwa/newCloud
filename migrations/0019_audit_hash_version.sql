-- The audit preimage gains a version (2026-10-24 review, M6).
--
-- The chain hashed everything except request_id and auth_method, so a database writer could rewrite
-- how an actor authenticated — session, app password, break-glass — and every verification would
-- still pass. A field that is recorded but not covered by the hash is worse than an unrecorded one,
-- because it reads as evidence.
--
-- It cannot be fixed by hashing the two fields for everyone: every existing row was hashed without
-- them, so changing the preimage outright would report the whole history as tampered with on
-- upgrade. Instead each row records which preimage it was sealed under, and the verifier uses that
-- row's rules.
--
-- DEFAULT 1 makes the migration a no-op for existing rows: they were written with version 1 and
-- keep verifying as version 1. New rows are written with 2, which appends the two fields.
ALTER TABLE audit.events
    ADD COLUMN IF NOT EXISTS hash_version smallint NOT NULL DEFAULT 1
        CHECK (hash_version >= 1);

-- Backfilling is deliberately absent: nothing distinguishes a row written before this migration
-- from one written after, except that the old ones cannot be re-hashed. Their version is 1 because
-- that is how they were computed, not because of a guess.
