-- 0033_document_pipeline_deltas — the Documentos milestone takes its first shape.
-- Reference: docs/erd-documentos.md §4 (deltas de document) and §6/§7 (saga de estados).
-- SCHEMA ONLY — zero slice logic (that is internal/document, the Fatia 1 slice).
--
-- Fatia 1 = upload + leitura + aba Documentos. document grows the pipeline-status and
-- upload-integrity deltas; chunk is left untouched here — its vector(1024) + embedding_model
-- + chunk_hash + the HNSW similarity index land with the embeddings fatia, co-located with
-- the code that uses them. document has 0 rows today, so the NOT NULL add is safe.
--
-- RLS: document already carries tenant_id + the tenant_isolation policy (0001) — no
-- isolation work needed here.

ALTER TABLE document
    -- explicit saga state: the file walks PENDING→UPLOADED→EXTRACTING→EXTRACTED→CHUNKED→
    -- READY (or FAILED), like the draft saga. Today the state is inferred from
    -- storage_key/extracted_at; a real column lets the aba show "processando…" and the
    -- pipeline retomar por estado (erd-documentos.md §6/§7). Born PENDING (pre-upload).
    ADD COLUMN status            text NOT NULL DEFAULT 'PENDING',
    -- upload integrity + dedup: checksum (sha256) catches a re-upload of identical bytes;
    -- mime_type/size_bytes are validated at presign and shown by the UI.
    ADD COLUMN mime_type         text,
    ADD COLUMN size_bytes        bigint,
    ADD COLUMN checksum          text,
    -- what the UI shows: title (editável) + the original filename as uploaded.
    ADD COLUMN title             text,
    ADD COLUMN original_filename text,
    -- why a stage failed (extract/OCR/embed) — { stage, message }; null while OK.
    ADD COLUMN error             jsonb,
    -- soft delete (erd-documentos.md §8): DELETE só apaga UPLOAD, e por auditoria
    -- marca deleted_at em vez de remover a linha; reads filtram deleted_at IS NULL.
    -- Um doc origin=COURT (dos autos) nunca é apagável — regra no slice, não aqui.
    ADD COLUMN deleted_at        timestamptz;

-- closed status set — reject a typo'd/unknown state at write time (mirrors the
-- deadline status CHECK). READY = advisory pode citar; FAILED = "reprocessar" na UI.
ALTER TABLE document
    ADD CONSTRAINT document_status_check
    CHECK (status IN ('PENDING','UPLOADED','EXTRACTING','EXTRACTED','CHUNKED','READY','FAILED'));
