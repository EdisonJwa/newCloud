-- External storage mounts (ADR-0014).
--
-- A mount is a pointer, not a copy: the mount-point folder is an ordinary nodes row (created
-- by the mount service as a top-level child of the owner's root), and this row is everything
-- the server remembers about the remote it points at. Contents below the mount point are
-- resolved live from the remote on every request; no nodes row is ever created for them,
-- which is the entire "no copy, no sync" property — there is nothing to drift because there
-- is nothing stored.
--
-- The direction of the foreign key is deliberate: mounts.node_id → nodes ON DELETE CASCADE.
-- Deleting the node row (unmount) takes the mount row with it; deleting the mount row cannot
-- silently strand an ordinary-looking folder that resolves to nothing. Unmounting goes
-- through the store's DeleteMount, which removes both rows in one transaction and refuses a
-- node that somehow grew children.
--
-- secret_enc is AES-256-GCM ciphertext produced with the key from mounts.secret_file. The
-- database never sees the plaintext credential, and a metadata backup alone cannot talk to
-- the remote. The key file has the same custody shape as the break-glass password file.
CREATE TABLE IF NOT EXISTS mounts (
    id            uuid PRIMARY KEY,
    node_id       uuid NOT NULL UNIQUE REFERENCES nodes (id) ON DELETE CASCADE,
    owner_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          text NOT NULL,
    scheme        text NOT NULL CHECK (scheme IN ('webdav', 'ftp', 'sftp', 'smb')),
    endpoint      text NOT NULL,
    root_path     text NOT NULL DEFAULT '',
    username      text NOT NULL DEFAULT '',
    secret_enc    bytea NOT NULL DEFAULT ''::bytea,
    read_only     boolean NOT NULL DEFAULT false,
    created_by    uuid NOT NULL REFERENCES users (id),
    health_status text NOT NULL DEFAULT 'unknown',
    health_detail text NOT NULL DEFAULT '',
    health_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Owner listings: the mount list a user sees is one indexed query.
CREATE INDEX IF NOT EXISTS mounts_owner_idx ON mounts (owner_id);

-- 'smb' is accepted by the CHECK so a deployment's config can name it, but no SMB provider is
-- compiled in this build (ADR-0014): dialing one refuses with a clear error rather than a 500.
