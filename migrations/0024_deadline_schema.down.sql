-- 0024_deadline_schema (down) — reverts the prazos schema to its pre-0024 state.
-- Order: drop the new tables first (task references deadline), then peel deadline
-- back to exactly its 0001 shape (drop the CHECK, restore DEFAULT 'OPEN', drop the
-- delta columns, disable RLS). DROP TABLE cascades its own policy/indexes.

DROP TABLE IF EXISTS task;
DROP TABLE IF EXISTS deadline_rule;

-- deadline: undo RLS, the status CHECK + PENDING default, and the delta columns,
-- leaving the table identical to its 0001 definition.
DROP POLICY IF EXISTS tenant_isolation ON deadline;
ALTER TABLE deadline DISABLE ROW LEVEL SECURITY;

ALTER TABLE deadline
    DROP CONSTRAINT IF EXISTS deadline_status_check,
    ALTER COLUMN status SET DEFAULT 'OPEN';

ALTER TABLE deadline
    DROP COLUMN IF EXISTS tenant_id,
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS source,
    DROP COLUMN IF EXISTS confirmed_by,
    DROP COLUMN IF EXISTS confirmed_at,
    DROP COLUMN IF EXISTS doubled_reason,
    DROP COLUMN IF EXISTS rules_version;
