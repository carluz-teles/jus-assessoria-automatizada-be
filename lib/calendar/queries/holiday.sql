-- lib/calendar queries (holiday) — reference calendar of non-working days.
-- Shared reference data, so no tenant_id filter. The forensic recess is a rule
-- in lib/calendar (inRecess), never rows, so it does not appear here.

-- name: IsHoliday :one
-- True when `date` is a registered holiday for the national calendar, the given
-- UF, or the given court. Empty uf/court never match their scope (STATE/COURT
-- rows always carry a non-empty scope_id).
SELECT EXISTS (
    SELECT 1 FROM holiday
    WHERE date = $1
      AND ( scope = 'NATIONAL'
         OR (scope = 'STATE' AND scope_id = $2)
         OR (scope = 'COURT' AND scope_id = $3) )
);

-- name: SeededNationalYears :many
-- Distinct years that already hold at least one NATIONAL holiday, so the seeder
-- fetches only the missing years.
SELECT DISTINCT EXTRACT(YEAR FROM date)::int AS year
FROM holiday
WHERE scope = 'NATIONAL';

-- name: UpsertHoliday :batchexec
-- Idempotent insert of one holiday; a duplicate (by scope + coalesce(scope_id)
-- + date) is ignored. NATIONAL rows pass a NULL scope_id.
INSERT INTO holiday (scope, scope_id, date, name)
VALUES ($1, $2, $3, $4)
ON CONFLICT (scope, coalesce(scope_id, ''), date) DO NOTHING;
