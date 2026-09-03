-- 0096: draft.content_edited — sinaliza que o content_html foi editado À MÃO pelo
-- advogado desde a última geração. Necessário porque mudar o conjunto de teses
-- (aprovar remoção/inclusão) REGENERA a peça pelo profile (a peça é coesa e fiel
-- ao template — recortar um trecho quebra coesão E estrutura), e regerar
-- sobrescreve os ajustes manuais. Com este flag o FE avisa antes de descartar
-- ("avisar e confirmar").
--
--   * autosave (SaveContentHtml) → content_edited = true;
--   * geração bem-sucedida → content_edited = false (peça nova, sem ajuste manual).
ALTER TABLE draft
    ADD COLUMN content_edited boolean NOT NULL DEFAULT false;
