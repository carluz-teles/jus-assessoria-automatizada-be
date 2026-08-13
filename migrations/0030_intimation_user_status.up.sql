-- 0030_intimation_user_status — the user-facing workflow state of an intimação,
-- SEPARATE from the existing `status` column. That column (0014) is the DJEN
-- CANCELLATION lifecycle (ACTIVE|CANCELLED): the sync upsert flips it on every
-- re-observation (ON CONFLICT DO UPDATE SET status = EXCLUDED.status) so the
-- deadline slice can revoke a retracted publication. Overloading it with the
-- triagem state (the user resolving/ignoring an intimação) would let a routine
-- re-observação clobber the user's decision back to PENDING — a correctness bug.
--
-- So the triagem state gets its OWN column. The upsert's INSERT column list does
-- NOT include user_status, so a first insert takes the DEFAULT 'PENDING' and a
-- re-observation's UPDATE branch never touches it — the user's resolve/ignore is
-- preserved structurally, no COALESCE guard needed.
--
--   • PENDING  — the default: a fresh (or reopened) intimação awaiting triagem;
--   • RESOLVED — the user marked it handled (POST .../resolve);
--   • IGNORED  — the user dismissed it (POST .../ignore).
--
-- Enum is text validated on the app (CHECK-on-app convention), never a CHECK in
-- the DB — same as `status`, `type`, `lifecycle`. NOT NULL DEFAULT backfills the
-- existing rows to PENDING in one statement. intimation already carries tenant_id
-- + the tenant_isolation RLS policy (inherited from `notification`, 0001); both
-- barriers still apply — this only adds the column and its counting index.
ALTER TABLE intimation
    ADD COLUMN user_status text NOT NULL DEFAULT 'PENDING';

-- Serves GET /v1/intimacoes/summary: the KPI counts group by user_status per
-- tenant. A composite (tenant_id, user_status) index lets the aggregate scan the
-- tenant's slice by bucket without touching the heap for the count.
CREATE INDEX intimation_tenant_user_status_idx ON intimation (tenant_id, user_status);
