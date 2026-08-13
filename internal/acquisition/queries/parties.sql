-- parties.sql (acquisition slice) — the process's partes (autor/réu/terceiro) and
-- their advogados, materialized from the DJEN so the cockpit renders the AUTOR/RÉU
-- cards. Two write paths (idempotent bulk upserts, run in the sync tx) and one read
-- (the /processos/:id/partes deep-read). All tenant-scoped (barrier 1) on top of RLS.

-- name: BatchUpsertParties :many
-- Upsert MANY parties in one round-trip from a jsonb array of {case_id, role, name}.
-- Idempotent on the natural key (tenant, case, role, name): a re-observation of the
-- same communication re-lands the same rows. ON CONFLICT DO UPDATE (a no-op touch of
-- name) rather than DO NOTHING so the RETURNING yields the row id whether it was just
-- inserted OR already existed — the caller needs every party's id to attach its
-- advogados. document stays NULL (v0 never discloses CPF/CNPJ); source is DJEN.
-- RETURNING role+name lets the caller map each party id back to its parsed counsels.
INSERT INTO party (tenant_id, case_id, role, name, source)
SELECT @tenant_id::uuid, r.case_id, r.role, r.name, 'DJEN'
FROM jsonb_to_recordset(@parties::jsonb) AS r(
    case_id uuid, role text, name text
)
ON CONFLICT (tenant_id, case_id, role, name) DO UPDATE SET name = EXCLUDED.name
RETURNING id, case_id, role, name;

-- name: BatchUpsertPartyCounsels :exec
-- Upsert MANY advogados in one round-trip from a jsonb array of {party_id, name, oab,
-- uf}. Idempotent on (tenant, party, oab, uf): ON CONFLICT DO NOTHING, so a re-observation
-- neither duplicates nor errors. source is DJEN. The name is refreshed only on insert
-- (a DO NOTHING leaves the existing row) — an advogado's OAB is the stable identity.
INSERT INTO party_counsel (tenant_id, party_id, name, oab, uf, source)
SELECT @tenant_id::uuid, r.party_id, r.name, r.oab, r.uf, 'DJEN'
FROM jsonb_to_recordset(@counsels::jsonb) AS r(
    party_id uuid, name text, oab text, uf text
)
ON CONFLICT (tenant_id, party_id, oab, uf) DO NOTHING;

-- name: ListPartiesByProcesso :many
-- The /processos/:id/partes deep-read: every party of the process behind a court_record
-- :id, with its advogados aggregated as a jsonb array (name/oab/uf), in one round-trip.
-- Isolation: court_record → court_case resolves the case, scoped by tenant_id (barrier 1);
-- a foreign or unknown :id resolves to no case and yields no rows. Ordered by role then
-- name so the caller buckets autor/réu/terceiro deterministically. counsels defaults to
-- an empty jsonb array (never NULL) when a party has no advogado.
SELECT p.id, p.role, p.name, p.document,
       -- Cast to text so pgx scans a JSON string (a plain jsonb column decodes into a Go
       -- map/slice for an interface{} target, not raw bytes); the repo unmarshals the text.
       COALESCE(
         (SELECT jsonb_agg(jsonb_build_object('name', pc.name, 'oab', pc.oab, 'uf', pc.uf)
                           ORDER BY pc.name)
          FROM party_counsel pc
          WHERE pc.party_id = p.id),
         '[]'::jsonb
       )::text AS counsels
FROM party p
WHERE p.tenant_id = @tenant_id::uuid
  AND p.case_id = (
      SELECT cr.case_id FROM court_record cr
      WHERE cr.id = @court_record_id::uuid AND cr.tenant_id = @tenant_id::uuid
  )
ORDER BY p.role, p.name;
