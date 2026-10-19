-- Bounded identity revalidation (ID-06, WC-GAP-001).
--
-- Role and group membership reach this database only when somebody signs in, because the OIDC
-- callback is the only place the IdP's claims are read. So a user removed from the admin group who
-- never signs in again keeps the old role in the `users` row, and the session and App Password they
-- already hold keep exercising it. "Takes effect at the next login" is not a bound on that window;
-- it is the statement that there is no bound.
--
-- idp_confirmed_at is when the IdP last asserted this identity's claims. The application compares it
-- against auth.revalidate_minutes on every authenticated request: past the window, the credential is
-- refused until the person signs in again. That turns an unbounded exposure into a documented one.
--
-- Nullable, and NULL means "never confirmed" rather than "confirmed at the epoch". A pre-existing row
-- migrated by this file has never been confirmed, and the check treats it as immediately stale —
-- which is correct: the deployment just acquired the ability to notice, and the first thing to notice
-- is that it knows nothing about these accounts yet. Deployments that leave revalidate_minutes at 0
-- are unaffected, and startup says so.
--
-- Break-glass rows are exempt in code, not in schema: they have no IdP to be confirmed against.
ALTER TABLE users ADD COLUMN IF NOT EXISTS idp_confirmed_at timestamptz;

-- The revalidation sweep and the per-request check both look for stale rows. A partial index keeps
-- that cheap on a table where most accounts are current.
CREATE INDEX IF NOT EXISTS users_idp_confirmed_idx ON users (idp_confirmed_at)
    WHERE idp_issuer IS NOT NULL;

COMMENT ON COLUMN users.idp_confirmed_at IS
    'When the IdP last asserted this identity''s role and group claims. NULL means never.';
