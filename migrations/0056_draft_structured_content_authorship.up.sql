-- Peça v2 — estrutura em blocos + autoria da peça (Fatia 1 + 2 do BE que
-- suporta o novo painel Iterar/Revisão do FE).
--
-- structured_content (jsonb) — fonte de verdade nova pra a estrutura da peça
-- (preâmbulo + N seções). O `content` (text) fica: continua sendo escrito em
-- dual-write com a serialização do estruturado, pra manter export/DOCX/PDF e
-- código legado funcionando sem breaking change. Novos drafts (pós-Fatia B do
-- generate) nascem com structured_content populado; drafts pré-migração ficam
-- NULL e o read model parseia on-the-fly (write-back best-effort).
--
-- Shape do jsonb (documentado aqui pra facilitar leitura via psql):
--   {
--     "preamble": { "paragraphs": ["...", "..."] },
--     "sections": [
--       { "id": "fatos", "roman": "I", "title": "Dos fatos",
--         "short_title": "Fatos", "paragraphs": ["...", "..."] }
--     ]
--   }
-- Sem CHECK constraint SQL — o parser server-side + a validação no handler
-- garantem shape. Fazer schema check em PG puro seria caro e frágil.
--
-- authorship — enum de 2 valores. "assistant" (default) = peça gerada/iterada
-- pela IA, o advogado pode pedir reescritas na aba Iterar. "human_taken" = o
-- advogado assumiu autoria manual, aba Iterar desaparece e Revisão aparece
-- (sugestões proativas que o advogado aplica/descarta). One-way por ora; um
-- dia pode virar bidirecional, mas não afeta o schema.

ALTER TABLE draft
    ADD COLUMN structured_content jsonb,
    ADD COLUMN authorship         text NOT NULL DEFAULT 'assistant';

ALTER TABLE draft
    ADD CONSTRAINT draft_authorship_check
    CHECK (authorship IN ('assistant', 'human_taken'));

-- Índice por tenant+authorship: futuras queries de "meus drafts em modo
-- humano" ou "peças ainda em iteração automática" ficam baratas. Cardinalidade
-- baixíssima do enum não justifica index dedicado, mas o par com tenant é
-- útil pra qualquer filtro tenant-scoped, RLS-alinhado.
CREATE INDEX draft_tenant_authorship_idx ON draft (tenant_id, authorship);
