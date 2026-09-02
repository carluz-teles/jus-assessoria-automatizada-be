-- 0094: suggested_thesis_anchor — MULTI-ÂNCORA das teses sugeridas (thesis_anchor
-- 1:N, docs/erd-tipos-de-peca.md §4). Até aqui a tese carregava UMA âncora só (os
-- campos singulares source_* em suggested_thesis). Vários autos que dizem a MESMA
-- coisa (ex.: várias certidões advertindo "extinção se não houver manifestação")
-- viravam teses semanticamente DUPLICADAS, cada uma com 1 âncora. Agora a tese é
-- consolidada (uma só) e carrega N âncoras — todos os documentos dos autos que a
-- sustentam.
--
--   * suggested_thesis_id ON DELETE CASCADE: as âncoras morrem com a tese (que por
--     sua vez morre com o draft/intimação — cascata em cadeia).
--   * tenant_id: isolamento (barreira 1 na app + RLS barreira 2), como toda tabela.
--   * document_id nullable: âncora sem documento (ancorada só no teor) — raro, mas
--     o modelo permite; NULL quando não resolveu para um chunk de documento.
--   * source_ref: o número do trecho (1..N) que o LLM citou e do qual esta âncora
--     foi resolvida (hits[source_ref-1]).
--   * grounded: a evidence desta âncora foi verificada como substring literal do
--     trecho citado.
--   * position: ordem determinística das âncoras dentro da tese (âncora primária = 0).
--
-- Os campos singulares source_* em suggested_thesis PERMANECEM (espelham a âncora
-- PRIMÁRIA) pra o FE atual não quebrar; a tabela nova é aditiva.
CREATE TABLE suggested_thesis_anchor (
    id                  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    suggested_thesis_id uuid        NOT NULL REFERENCES suggested_thesis(id) ON DELETE CASCADE,
    tenant_id           uuid        NOT NULL REFERENCES tenant(id),
    document_id         uuid        NULL,
    page                int         NOT NULL DEFAULT 0,
    excerpt             text        NOT NULL DEFAULT '',
    label               text        NOT NULL DEFAULT '',
    source_ref          int         NOT NULL DEFAULT 0,
    grounded            boolean     NOT NULL DEFAULT false,
    position            int         NOT NULL DEFAULT 0,
    created_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX suggested_thesis_anchor_thesis_id_idx
    ON suggested_thesis_anchor (suggested_thesis_id);
