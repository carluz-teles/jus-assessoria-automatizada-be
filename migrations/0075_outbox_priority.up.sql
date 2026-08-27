-- outbox drain priority: interactive (0) events jump ahead of bulk (1) so a mass
-- backfill (thousands of acquisition.*/deadline.* rows) never starves an event a
-- user is waiting on (draft.generation_requested, filing.*, onboarding, notifications).
-- Written at Publish time by priorityFor(type); NULL/default = 1 (background, fail-safe).
ALTER TABLE outbox ADD COLUMN priority smallint NOT NULL DEFAULT 1;

CREATE INDEX outbox_unpublished_priority_idx ON outbox (priority, id) WHERE published_at IS NULL;
DROP INDEX outbox_id_idx;
