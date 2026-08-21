-- Responsáveis por intimação (handoff-v2 §2 "dois papéis por intimação"):
-- conductor_user_id → condutor do prazo (quem executa/acompanha)
-- reviewer_user_id  → responsável pela revisão e assinatura
-- Ambos são nullable (sem atribuição = UI mostra "A atribuir"). Sem ON DELETE CASCADE:
-- a remoção de um membro não pode apagar silenciosamente o vínculo histórico — o slice
-- de offboarding (futuro) faz a limpeza explícita. Os índices cobrem a FK como exigido
-- pelo PostgreSQL-table-design (FK columns nunca auto-indexadas).
ALTER TABLE intimation
  ADD COLUMN conductor_user_id uuid REFERENCES app_user(id),
  ADD COLUMN reviewer_user_id  uuid REFERENCES app_user(id);

CREATE INDEX ON intimation (conductor_user_id) WHERE conductor_user_id IS NOT NULL;
CREATE INDEX ON intimation (reviewer_user_id)  WHERE reviewer_user_id  IS NOT NULL;
