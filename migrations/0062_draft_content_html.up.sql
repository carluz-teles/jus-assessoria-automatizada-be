-- Fase B do editor rico: coluna content_html carrega a peça em HTML
-- rico (formato Tiptap) — preserva formatação inline (bold/italic/color/
-- font/tabelas) que o structured_content JSON não guarda. Convive com
-- structured_content: a IA continua gerando o JSON semântico (sections
-- por Fatos/Direito/Pedidos), e o read model derivá o HTML dele quando
-- content_html for NULL (lazy migration). Save do editor grava direto
-- em content_html; a partir daí ele vira source of truth pro renderer.
--
-- NULL permitido:
--   - peças legacy antes desta migration
--   - peças recém-criadas pela IA antes de qualquer edição humana
--     (structured_content é o único artefato até o 1º save do editor)
ALTER TABLE draft
    ADD COLUMN content_html text;

COMMENT ON COLUMN draft.content_html IS
  'HTML rico do editor Tiptap. NULL = usar structured_content como fonte.';
