-- 0034_chunk_embedding_deltas (down) — peel chunk back to its 0001 shape.
-- Drop the two indexes this migration created (the HNSW cosine index and the dedup unique
-- index) before dropping the columns they cover, then drop the provenance/integrity columns,
-- then restore the embedding column at its original vector(1536) dimensionality.
--
-- The 0001 chunk (document_id, page) index predates this migration and is untouched.

-- Drop the dedup unique index (named by the DB: chunk_document_id_page_chunk_hash_idx) and the
-- HNSW cosine index (chunk_embedding_idx). Guarded so the down is re-runnable.
DROP INDEX IF EXISTS chunk_document_id_page_chunk_hash_idx;
DROP INDEX IF EXISTS chunk_embedding_idx;

ALTER TABLE chunk
    DROP COLUMN IF EXISTS embedding_model,
    DROP COLUMN IF EXISTS dim,
    DROP COLUMN IF EXISTS chunk_hash;

-- Restore the original dimensionality (0001 was vector(1536)); drop + re-add, same as the up.
ALTER TABLE chunk DROP COLUMN IF EXISTS embedding;
ALTER TABLE chunk ADD COLUMN embedding vector(1536);
