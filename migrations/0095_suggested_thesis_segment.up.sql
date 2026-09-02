-- 0095: suggested_thesis_segment — mapeia cada tese sugerida ao TRECHO da peça
-- gerada que ela produziu (thesis → draft_segment, docs/erd-tipos-de-peca.md §4).
--
-- Problema: ao propor a REMOÇÃO de uma tese, a moldura (FE) só mostrava a
-- descrição truncada da tese — não "em qual mudança isso vai implicar", porque
-- não havia vínculo persistido tese↔parágrafo da minuta. A geração produz um
-- markdown único → HTML, sem segmentação.
--
-- Solução (match pós-geração, determinístico): depois de gerar o HTML, casamos
-- cada tese selecionada com a SEÇÃO cujo heading espelha o label da tese (a
-- geração por profile já emite um subtítulo por tese). O texto daquela seção é
-- persistido aqui; o FE mostra o trecho real ao propor a remoção.
--
-- O ERD modelou `draft_segment` referenciando a tabela `thesis` LEGADA (0085),
-- que não usamos — as teses reais vivem em `suggested_thesis` (0092/0093). Mesmo
-- precedente do multi-âncora (0094 criou suggested_thesis_anchor em vez de reusar
-- thesis_anchor): esta tabela referencia suggested_thesis.
--
--   * suggested_thesis_id ON DELETE CASCADE: segmentos morrem com a tese (que
--     morre com o draft — cascata em cadeia). Cobre o re-suggest de teses.
--   * draft_id ON DELETE CASCADE + coluna direta: permite o DELETE por draft na
--     regeração da peça (sem depender do JOIN com suggested_thesis).
--   * tenant_id: isolamento por app-filter (barreira 1), espelhando 0094.
--   * heading: título da seção (display) — o FE casa por texto pra ancorar/rolar.
--   * conteudo: texto dos parágrafos da seção (o trecho a exibir na remoção).
--   * position: ordem determinística.
CREATE TABLE suggested_thesis_segment (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    suggested_thesis_id uuid        NOT NULL REFERENCES suggested_thesis(id) ON DELETE CASCADE,
    tenant_id           uuid        NOT NULL REFERENCES tenant(id),
    draft_id            uuid        NOT NULL REFERENCES draft(id) ON DELETE CASCADE,
    heading             text        NOT NULL DEFAULT '',
    conteudo            text        NOT NULL DEFAULT '',
    position            int         NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX suggested_thesis_segment_thesis_id_idx
    ON suggested_thesis_segment (suggested_thesis_id);
CREATE INDEX suggested_thesis_segment_draft_id_idx
    ON suggested_thesis_segment (tenant_id, draft_id);
