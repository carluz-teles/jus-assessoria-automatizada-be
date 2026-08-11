-- publication is the national DJEN firehose landing zone: every communication
-- published in the Diário de Justiça Eletrônico Nacional, ingested by tribunal/day
-- and matched to tenants' watched OABs LOCALLY (no per-OAB API sweep). It is system
-- reference data — national and tenant-agnostic — so, like holiday, it carries no
-- tenant_id and no RLS. Dedup is by the DJEN hash (globally unique per communication).
-- Retention is bounded (~90d) via made_available_at; a scheduled prune drops old rows.
-- Scale path (deferred to its own slice): convert to RANGE partitioning by
-- made_available_at when volume/pruning demands it — the read/write interface is
-- unchanged, and partition lifecycle is the scheduler's concern, not the schema's.
CREATE TABLE publication (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    hash              text NOT NULL,       -- DJEN hash: globally unique per communication
    court             text NOT NULL,       -- siglaTribunal (ex.: TJSP, TRT15, TRE-SP)
    cnj_number        text NOT NULL,       -- numero_processo
    made_available_at date NOT NULL,       -- data_disponibilizacao (match window + retention)
    recipient_oabs    text[] NOT NULL,     -- normalized "NUMBER|UF" keys for the local OAB match
    payload           jsonb NOT NULL,      -- raw DJEN item; the existing djen parser consumes it
    ingested_at       timestamptz NOT NULL DEFAULT now()
);

-- Dedup + idempotent re-ingestion (the daily lookback re-fetches the same days):
-- the natural key is the DJEN hash. Doubles as the ON CONFLICT (hash) target.
CREATE UNIQUE INDEX publication_hash_key ON publication (hash);

-- The local match: a tenant's watched OAB ∈ recipient_oabs, via array overlap (&&) /
-- containment (@>). GIN is the index that accelerates those on text[].
CREATE INDEX publication_recipient_oabs_gin ON publication USING GIN (recipient_oabs);

-- Retention prune and the onboarding history match both scan by disponibilização date.
CREATE INDEX publication_made_available_at_idx ON publication (made_available_at);
