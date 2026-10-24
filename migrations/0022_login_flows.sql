-- Pending Nextcloud login-flow-v2 authorizations (ADR-0015).
--
-- A desktop or mobile client starts the flow anonymously and receives two independent
-- secrets: a POLL token it keeps, and a LOGIN token it opens in the user's browser. The
-- browser authenticates with the normal OIDC session and approves the client; the client
-- then polls with its own token and receives a freshly minted App Password.
--
-- Only the SHA-256 of either token is stored, for the same reason App Passwords are
-- hashed: a metadata dump must not yield a usable capability. The row is written before
-- the browser step and DELETED by the first successful poll, so two pollers cannot both
-- receive a credential.
--
-- user_id stays NULL until the browser approves, and CASCADE means deleting the account
-- also discards any pending authorisation for it.
CREATE TABLE IF NOT EXISTS login_flows (
    id               uuid PRIMARY KEY,
    poll_token_hash  bytea NOT NULL UNIQUE,
    login_token_hash bytea NOT NULL UNIQUE,
    user_id          uuid REFERENCES users (id) ON DELETE CASCADE,
    client_name      text NOT NULL DEFAULT '',
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    approved_at      timestamptz
);

-- The sweep of expired rows, and the "is this token still within its window" read.
CREATE INDEX IF NOT EXISTS login_flows_expires_idx ON login_flows (expires_at);
