-- publication store queries (acquisition slice). The national DJEN firehose: the
-- ingestion sweeps the diário by tribunal/day and lands every communication here,
-- to be matched to tenants' watched OABs locally. No tenant scope (national reference
-- data, like holiday), so no RLS and no tenant filter.

-- name: InsertPublication :one
-- Land one national communication, idempotent on the DJEN hash (globally unique per
-- communication). The daily lookback re-fetches recent days, so ON CONFLICT (hash)
-- DO NOTHING makes a re-ingest a no-op: the loser gets no row (pgx.ErrNoRows), which
-- the caller reads as "already ingested" to tally new vs. deduped. recipient_oabs is
-- the normalized "NUMBER|UF" key set for the local OAB match; payload is the raw item.
INSERT INTO publication
    (hash, court, cnj_number, made_available_at, recipient_oabs, payload)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (hash) DO NOTHING
RETURNING id;
