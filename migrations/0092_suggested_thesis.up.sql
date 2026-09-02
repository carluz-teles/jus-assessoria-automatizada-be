-- 0092: suggested_thesis — persiste as teses sugeridas (Sugerir Teses, C1). Até
-- aqui a sugestão era STATELESS (gerada in-memory a cada POST, sem id/estado); o
-- FE (pecas-v2) precisa das teses PERSISTIDAS com id/state/position pra
-- selecionar/manter a escolha do advogado entre revisitas da tela.
--
-- Escopo C1: DRAFT-scoped (draft_id NOT NULL). A partida (fluxo /pecas/nova, sem
-- draft ainda) é intimation-scoped e vem na C2 — que adiciona intimation_id via
-- outra migration e relaxa a obrigatoriedade do draft_id. Por isso os campos
-- espelham 1:1 a entidade Thesis (label/confidence/reference/foundation/evidence
-- + source_*) já resolvida pela geração (RAG+LLM), acrescidos de state/position.
--
--   * draft_id ON DELETE CASCADE: as teses de um draft morrem com ele.
--   * evidence text[]: trechos literais que sustentam a tese (espelha Thesis.Evidence).
--   * source_document_id nullable: "" quando ancorada só no teor/doutrina (sem chunk).
--   * state: máquina de estados da seleção (off → pending_add → included →
--     pending_remove). CHECK trava o conjunto fechado na borda do banco.
--   * position: ordem determinística já aplicada na geração (sortTheses).
CREATE TABLE suggested_thesis (
    id                 uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          uuid        NOT NULL REFERENCES tenant(id),
    draft_id           uuid        NOT NULL REFERENCES draft(id) ON DELETE CASCADE,
    label              text        NOT NULL,
    confidence         text        NOT NULL,
    reference          text        NOT NULL DEFAULT '',
    foundation         text        NOT NULL DEFAULT '',
    evidence           text[]      NOT NULL DEFAULT '{}',
    source_ref         int         NOT NULL DEFAULT 0,
    source_document_id uuid        NULL,
    source_page        int         NOT NULL DEFAULT 0,
    source_excerpt     text        NOT NULL DEFAULT '',
    source_label       text        NOT NULL DEFAULT '',
    grounded           boolean     NOT NULL DEFAULT false,
    state              text        NOT NULL DEFAULT 'off'
        CHECK (state IN ('off', 'pending_add', 'included', 'pending_remove')),
    position           int         NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX suggested_thesis_draft_id_idx ON suggested_thesis (draft_id);
