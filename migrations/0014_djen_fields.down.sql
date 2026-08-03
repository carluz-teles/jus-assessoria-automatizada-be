-- Reverso da 0014_djen_fields — dropa as colunas DJEN na ordem inversa da adição.
ALTER TABLE court_record
    DROP COLUMN judging_body;

ALTER TABLE intimation
    DROP COLUMN cancel_reason,
    DROP COLUMN cancelled_at,
    DROP COLUMN source_url,
    DROP COLUMN status,
    DROP COLUMN type;
