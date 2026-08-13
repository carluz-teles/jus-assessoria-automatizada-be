-- 0034_chunk_embedding_deltas — the embeddings/retrieval fatia (Fatia 7) reshapes chunk.
-- Reference: docs/erd-documentos.md §7 (retrieval/pgvector) + Milestone Documentos decisions
-- (Voyage voyage-3.5-lite / vector(1024) for embeddings). SCHEMA ONLY — the slice logic lives
-- in internal/indexing (chunking + embed + INSERT + SearchChunks).
--
-- 0033 deliberately left chunk untouched, promising its vector(1024) + embedding_model +
-- chunk_hash + the HNSW similarity index would land "co-located with the code that uses them".
-- This is that migration.
--
-- WHY vector(1024) (not the 0001 vector(1536)): embeddings are produced by Voyage
-- voyage-3.5-lite, whose native output_dimension is 1024 (the milestone chose Voyage over
-- OpenAI/Anthropic — Anthropic has no embeddings API). pgvector CANNOT alter a vector column's
-- dimensionality in place, so the column is DROPPED and re-ADDED at 1024. chunk has 0 rows
-- today, so dropping the column loses nothing.
--
-- RLS: chunk carries NO tenant_id (0001) — it is reached only THROUGH document, and tenant
-- isolation on retrieval is enforced by JOINing document and filtering document.tenant_id
-- (SearchChunks). No isolation work is needed on chunk itself here.

-- Re-dimension the embedding: pgvector can't ALTER a vector's dim in place, so drop + re-add.
-- chunk has 0 rows, so no data is lost.
ALTER TABLE chunk DROP COLUMN embedding;
ALTER TABLE chunk ADD COLUMN embedding vector(1024);

-- Provenance + integrity of each chunk's vector:
--   embedding_model — the model id that produced the vector (e.g. "voyage-3.5-lite"), so a
--     re-embed with a different model is auditable and a future migration can target the stale rows.
--   dim            — the vector's dimensionality, denormalized for a cheap sanity check / stat.
--   chunk_hash     — sha256(text), the dedup key: the same page text re-extracted/reprocessed
--     produces the same hash, so the pipeline's INSERT ... ON CONFLICT DO NOTHING is idempotent.
ALTER TABLE chunk
    ADD COLUMN embedding_model text,
    ADD COLUMN dim             int,
    ADD COLUMN chunk_hash      text;

-- Similarity index: cosine distance (`embedding <=> $query`) over the HNSW graph — the read
-- path SearchChunks orders by this operator, so the index turns the retrieval scan into an ANN
-- lookup. vector_cosine_ops matches the <=> operator the query uses.
CREATE INDEX ON chunk USING hnsw (embedding vector_cosine_ops);

-- Dedup uniqueness: one row per (document_id, page, chunk_hash). This is the ON CONFLICT target
-- the pipeline INSERTs against, so reprocessing a document (a retry, a re-extract) never
-- duplicates chunks — the same text on the same page is written at most once.
CREATE UNIQUE INDEX ON chunk (document_id, page, chunk_hash);
