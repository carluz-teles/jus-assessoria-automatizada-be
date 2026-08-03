-- read-model queries (acquisition slice) — the screen reads, kept OFF the write
-- path (docs: "leitura de tela usa read model, DTO por query dedicada"). Each is
-- tenant-scoped (barrier 1) and keyset-paginated on a stable (sort_key, id) pair
-- so paging is offset-free and stable under concurrent inserts. The caller passes
-- a sentinel cursor for the first page, so there is no conditional WHERE.

-- name: ListProcessos :many
-- The consolidated processes screen: the tenant's live court records (SUPERSEDED
-- placeholders drop out), each with its most recent andamento for the "last
-- movement" column. Ordered by cnj_number then id (ascending keyset): the first
-- page passes ('', zero-uuid).
SELECT cr.id, cr.case_id, cr.cnj_number, cr.court, cr.degree, cr.class, cr.subject,
       cr.judging_body, cr.filed_at, cr.secrecy, cr.lifecycle, cr.completeness,
       COALESCE(m.text, '') AS last_movement_text, m.occurred_at AS last_movement_at
FROM court_record cr
LEFT JOIN LATERAL (
    SELECT de.text, de.occurred_at
    FROM docket_entry de
    WHERE de.court_record_id = cr.id
    ORDER BY de.occurred_at DESC
    LIMIT 1
) m ON true
WHERE cr.tenant_id = $1
  AND cr.lifecycle = 'ACTIVE'
  AND (cr.cnj_number, cr.id) > (@last_cnj::text, @last_id::uuid)
ORDER BY cr.cnj_number, cr.id
LIMIT $2;

-- name: ListIntimacoes :many
-- The intimações inbox: the tenant's intimations, newest availability first, with
-- the court record's number/court/degree joined in. Descending keyset on
-- (made_available_at, id); the first page passes the max sentinel
-- ('9999-12-31', max-uuid).
SELECT i.id, i.made_available_at, i.published_at, i.deadline_start_at,
       i.content, i.type, i.status, i.source, i.source_url,
       cr.cnj_number, cr.court, cr.degree
FROM intimation i
JOIN court_record cr ON cr.id = i.court_record_id
WHERE i.tenant_id = $1
  AND (i.made_available_at, i.id) < (@last_made_available::date, @last_id::uuid)
ORDER BY i.made_available_at DESC, i.id DESC
LIMIT $2;
