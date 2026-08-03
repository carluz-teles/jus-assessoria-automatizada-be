-- 0012_holiday.up.sql — reference calendar of non-working days for deadline math.
-- Faithful to docs/erd-modelo-de-dados.md (source of truth for the schema).
-- Reference data (NOT per-tenant): consumed by lib/calendar to derive
-- published_at/deadline_start_at (intimation) and end_date (deadline). The
-- forensic recess (20/12–20/01, CPC art. 220) is a fixed rule in lib/calendar,
-- not rows here.

CREATE TABLE holiday (
  id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  scope    text NOT NULL,          -- NATIONAL | STATE | COURT
  scope_id text,                   -- null p/ NATIONAL; UF p/ STATE; sigla do tribunal p/ COURT
  date     date NOT NULL,
  name     text NOT NULL
);

-- Dedup key. coalesce(scope_id,'') so NATIONAL rows (scope_id NULL) dedup by
-- (scope, date): a plain UNIQUE treats every NULL as distinct and would let the
-- national seeder insert duplicates. Doubles as the ON CONFLICT target for the
-- idempotent seeders.
CREATE UNIQUE INDEX holiday_scope_scopeid_date_key
  ON holiday (scope, coalesce(scope_id, ''), date);

-- Hot path: lib/calendar looks up by date (IsBusinessDay), narrowing scope after.
CREATE INDEX holiday_date_idx ON holiday (date);
