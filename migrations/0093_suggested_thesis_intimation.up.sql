-- 0093: generaliza suggested_thesis pra intimation-scope (C2 — fluxo da PARTIDA,
-- /pecas/nova, teses sugeridas ANTES do draft existir).
--
-- C1 (0092) criou a tabela DRAFT-scoped (draft_id NOT NULL). A partida sugere teses
-- ligadas à INTIMAÇÃO (ainda não há draft — o FE difere a criação até "Gerar"). Ao
-- promover partida→construção, copiamos as teses + a seleção pro draft (draft-scoped).
--
-- Regra: EXATAMENTE UM de (draft_id, intimation_id) é não-nulo — uma linha pertence
-- a um draft OU a uma intimação, nunca aos dois nem a nenhum. O CHECK trava isso na
-- borda do banco (mesma disciplina do state CHECK).
--
--   * intimation_id ON DELETE CASCADE: as teses da partida morrem com a intimação.
--   * draft_id vira NULLABLE (era NOT NULL na 0092) — as intimation-scoped não têm draft.
ALTER TABLE suggested_thesis
    ADD COLUMN intimation_id uuid NULL REFERENCES intimation(id) ON DELETE CASCADE;

ALTER TABLE suggested_thesis
    ALTER COLUMN draft_id DROP NOT NULL;

ALTER TABLE suggested_thesis
    ADD CONSTRAINT suggested_thesis_scope_chk
    CHECK ((draft_id IS NOT NULL)::int + (intimation_id IS NOT NULL)::int = 1);

CREATE INDEX suggested_thesis_intimation_id_idx ON suggested_thesis (intimation_id);
