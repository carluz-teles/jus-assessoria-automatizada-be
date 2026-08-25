-- Coluna created_by no draft — quem começou a peça. Preenchida no CriarPeca a
-- partir do principal.UserID (Clerk) e usada pelo read model da lista de peças
-- pra mostrar avatar/nome do responsável no card sem 2ª chamada.
--
-- Nullable pra não quebrar linhas antigas (backfill posterior se quisermos, mas
-- não é obrigatório — o FE mostra "—" quando vazio, e a peça continua
-- funcional sem o campo).
--
-- ON DELETE SET NULL — se o autor sair do escritório, a peça sobrevive sem
-- autor exibido (o dado histórico do processo permanece).

ALTER TABLE draft
  ADD COLUMN IF NOT EXISTS created_by uuid REFERENCES app_user(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS draft_tenant_created_by_idx ON draft (tenant_id, created_by);
