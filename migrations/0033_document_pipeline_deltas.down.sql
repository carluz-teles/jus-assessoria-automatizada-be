-- 0033_document_pipeline_deltas (down) — peel document back to its 0001 shape.
-- Drop the CHECK first (it references status), then the delta columns. The 0001
-- tenant_id/RLS/court_record_id index are untouched (they predate this migration).

ALTER TABLE document DROP CONSTRAINT IF EXISTS document_status_check;

ALTER TABLE document
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS mime_type,
    DROP COLUMN IF EXISTS size_bytes,
    DROP COLUMN IF EXISTS checksum,
    DROP COLUMN IF EXISTS title,
    DROP COLUMN IF EXISTS original_filename,
    DROP COLUMN IF EXISTS error,
    DROP COLUMN IF EXISTS deleted_at;
