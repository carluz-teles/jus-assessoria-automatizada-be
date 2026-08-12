-- deadline slice queries (the prazos CREATION path). Every write and read runs inside
-- the use case's transaction so RLS scopes it to the event's tenant (barrier 2) on top
-- of the explicit tenant filter (barrier 1). Absence is a typed error at the mapper,
-- never (nil, nil).

-- name: GetCourtRecordClass :one
-- Read the rito signal (class) for the record the intimação hangs on — the one
-- cross-table read the derivation needs (decisão P1: read the table, never import the
-- acquisition package). A missing record → pgx.ErrNoRows → typed not-found at the
-- mapper. class itself may be NULL (mapped to "").
SELECT class
FROM court_record
WHERE id = $1;

-- name: ResolveDeadlineRule :one
-- Resolve the conservative rule for (intimation_type, court) in a rules version. The
-- resolution lives HERE, in SQL (decisão travada, erd-prazos.md §8/§11): the most
-- SPECIFIC active match wins, falling back to the '*' catch-all — so an unknown type
-- still yields the safe GENERICO rule. Ordering, most specific first:
--   1. an exact intimation_type beats the '*' catch-all;
--   2. a court-specific rule (court_prefix set) beats an any-court rule;
--   3. a longer court_prefix is more specific than a shorter one;
--   4. finally, higher priority wins.
-- $1 = rules_version, $2 = intimation_type, $3 = court. A court_prefix matches when the
-- court sigla starts with it ($3 LIKE prefix || '%'); a NULL prefix matches any court.
SELECT rules_version, kind, days, counting, doubled
FROM deadline_rule
WHERE rules_version = $1
  AND active
  AND (intimation_type = $2 OR intimation_type = '*')
  AND (court_prefix IS NULL OR $3 LIKE court_prefix || '%')
ORDER BY (intimation_type = $2) DESC,
         (court_prefix IS NOT NULL) DESC,
         length(coalesce(court_prefix, '')) DESC,
         priority DESC
LIMIT 1;

-- name: InsertDeadline :one
-- Persist the derived prazo, BORN PENDING (status), source RULE. Idempotent on the 1:1
-- notification_id (UNIQUE): ON CONFLICT DO NOTHING yields NO row on a re-derivation, so
-- the mapper reads pgx.ErrNoRows as "already exists" (ErrDeadlineExists) instead of
-- poisoning the tx with a constraint error. confirmed_by/at stay NULL (no human aval
-- yet — that is the F2 slice). Returns the DB-assigned id; the repo maps the rest from
-- the input entity.
INSERT INTO deadline (
    tenant_id, court_record_id, notification_id,
    start_date, end_date, days, counting, doubled, doubled_reason,
    holidays_applied, status, source, kind, rules_version
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7, $8, $9,
    $10, $11, $12, $13, $14
)
ON CONFLICT (notification_id) DO NOTHING
RETURNING id;
