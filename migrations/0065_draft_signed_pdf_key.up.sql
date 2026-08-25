-- 0065_draft_signed_pdf_key — Fatia 2b: assinatura real de PDF (PAdES via GCP
-- KMS + digitorus/pdfsign). O PDF assinado vive no object storage; o draft
-- guarda só o ponteiro. NULL antes de assinar.
--
-- Path convention: {tenant_id}/pecas/{draft_id}/signed.pdf (tenant-scoped
-- como o resto do storage — sem confusão de dono no bucket).
ALTER TABLE draft
  ADD COLUMN signed_pdf_key text;
