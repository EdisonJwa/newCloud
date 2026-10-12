-- §4.3 retained usage: trash is separated from the logical quota.
--
-- The logical quota is defined as the current version of user-VISIBLE files. Trashed files are not
-- visible, so they belong to RETAINED usage, which the requirement caps separately. Neither backend
-- implemented this: trashing a file left its bytes counted against the quota, so a user who emptied
-- their bin was still told they were full.
--
-- A separate counter rather than recomputing on demand: the quota check runs on every write, and a
-- SUM over the trashed subtree at that point would be a scan in the hot path.

ALTER TABLE users ADD COLUMN trash_bytes bigint NOT NULL DEFAULT 0;
