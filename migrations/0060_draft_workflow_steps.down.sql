-- Reverte 0060: dropa as 3 colunas. Destrutivo — perde os timestamps do
-- workflow (não recuperáveis).
ALTER TABLE draft
  DROP COLUMN sent_to_signing_at,
  DROP COLUMN filing_number,
  DROP COLUMN filed_at;
