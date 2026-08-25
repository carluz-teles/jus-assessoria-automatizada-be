-- Reverte 0061: dropa signed_pdf_key. O blob no storage fica órfão (GC manual
-- se aplicado).
ALTER TABLE draft DROP COLUMN signed_pdf_key;
