-- Make the INSERT-only application role usable, so the audit restriction is real.
--
-- The first migration created `newcloud_app` with the right grants — SELECT/INSERT/UPDATE
-- on the public tables and SELECT/INSERT only on the audit tables — but the role is
-- NOLOGIN and the deployment connects as the database owner. The grants therefore protect
-- nothing: every connection is a superuser, and a superuser bypasses them.
--
-- That is a claim the documentation has been making since the first version ("the audit log
-- is append-only to the application"), and it was untrue in every deployment. This
-- migration turns the role into one a deployment can actually log in as, so the claim
-- becomes testable.
--
-- What is deliberately NOT done here:
--   * No password. A password in a migration would be in version control forever, and every
--     environment would share it. The operator sets one (see deploy/README or the bootstrap
--     snippet in docs), or uses certificate/IAM auth.
--   * No pg_hba changes. Those are cluster configuration, not schema, and silently widening
--     who may connect is not a migration's business.
--   * The role is not granted membership in the superuser. It stays unprivileged.

DO $$
BEGIN
    -- LOGIN only; still no CREATEDB, CREATEROLE, SUPERUSER or BYPASSRLS.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'newcloud_app') THEN
        ALTER ROLE newcloud_app LOGIN;
    ELSE
        CREATE ROLE newcloud_app LOGIN;
    END IF;
END
$$;

-- Re-apply the grants for anything created after 0001, and re-assert the audit restriction.
--
-- The grants at the bottom of 0001 use ON ALL TABLES, which only covers tables existing at
-- the time it ran. A migration that adds a table would otherwise ship a table the
-- application cannot use — a failure that shows up as a permission error in production
-- rather than in review.
GRANT USAGE ON SCHEMA public TO newcloud_app;
GRANT USAGE ON SCHEMA audit TO newcloud_app;

GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO newcloud_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO newcloud_app;

-- The audit log: INSERT and SELECT only. Re-stated here because this is the migration that
-- makes the role usable, and a claim worth making twice is worth re-asserting where it
-- takes effect.
GRANT INSERT, SELECT ON audit.events TO newcloud_app;
GRANT INSERT, SELECT ON audit.anchors TO newcloud_app;
REVOKE UPDATE, DELETE, TRUNCATE ON audit.events FROM newcloud_app;
REVOKE UPDATE, DELETE, TRUNCATE ON audit.anchors FROM newcloud_app;

-- Partitions do not inherit grants from the parent, so a partition created later would be
-- invisible to the application. This grants them directly.
--
-- The helper function from 0001 is not reused because it selects every partition-flagged
-- object in the audit schema, and an index partition carries that flag too — GRANT on an
-- index fails with "is an index" and aborts the whole migration. Selecting only relations
-- of kind 'r' (ordinary table) fixes it here; the function itself is corrected below so a
-- later partition-creation job does not hit the same wall.
GRANT INSERT, SELECT ON audit.events TO newcloud_app;
GRANT INSERT, SELECT ON audit.anchors TO newcloud_app;
DO $$
DECLARE
    part record;
BEGIN
    FOR part IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'audit'
          AND c.relispartition
          AND c.relkind = 'r'
    LOOP
        EXECUTE format('GRANT INSERT, SELECT ON audit.%I TO newcloud_app', part.relname);
    END LOOP;
END
$$;

-- Fix the 0001 helper so the same mistake cannot recur: an index partition is not a table
-- and cannot be granted on. Without this, the task that creates next month's partition
-- would abort.
CREATE OR REPLACE FUNCTION audit.grant_partition_privileges() RETURNS void
LANGUAGE plpgsql AS $fn$
DECLARE
    part record;
BEGIN
    FOR part IN
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'audit'
          AND c.relispartition
          AND c.relkind = 'r'
    LOOP
        EXECUTE format('GRANT INSERT, SELECT ON audit.%I TO newcloud_app', part.relname);
    END LOOP;
END
$fn$;

-- A view naming the one thing an operator needs to check after deploying: whether the
-- application is actually connected as the restricted role.
CREATE OR REPLACE VIEW app_role_status AS
SELECT
    current_user                                        AS connected_as,
    (SELECT rolsuper FROM pg_roles WHERE rolname = current_user) AS is_superuser,
    -- True when the audit log is genuinely append-only for this connection.
    (NOT COALESCE((SELECT rolsuper FROM pg_roles WHERE rolname = current_user), true)) AS audit_is_append_only;

COMMENT ON VIEW app_role_status IS
    'Whether this connection can rewrite the audit log. is_superuser=true means the INSERT-only grants are not protecting anything.';
