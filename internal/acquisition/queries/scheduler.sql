-- scheduler (re-poll) queries (acquisition slice).
-- The re-poll scheduler runs system-scoped (DoSystem sets app.system='on', so the
-- court_record RLS system escape hatch from migration 0016 exposes every tenant's
-- rows). Each tick it finds records whose next_sync_at is due, enqueues a fresh
-- court_record_observed for each (the enrichment consumer refreshes the DATAJUD
-- movimentos), and claims them by pushing next_sync_at forward so a later tick does
-- not re-enqueue the same record before the resync lands.

-- name: DueCourtRecordsForResync :many
-- Records due for a DATAJUD re-poll: ACTIVE only (SUPERSEDED placeholders and
-- archived processes drop out) with a past next_sync_at. Served by the partial
-- index on next_sync_at WHERE lifecycle='ACTIVE'.
SELECT id, tenant_id, case_id, cnj_number, degree, court
FROM court_record
WHERE lifecycle = 'ACTIVE' AND next_sync_at IS NOT NULL AND next_sync_at <= now()
ORDER BY next_sync_at
LIMIT $1;

-- name: ClaimCourtRecordResync :exec
-- Push a record's next_sync_at forward as its re-poll is enqueued, so the next tick
-- does not re-enqueue it; if the resync never lands, it falls due again after the
-- interval (at-least-once).
UPDATE court_record
SET next_sync_at = $2
WHERE id = $1;
