-- Consolida os dois papéis da intimação (conductor_user_id + reviewer_user_id — 0050)
-- em UM ÚNICO responsável. Decisão de produto: a distinção condutor-do-prazo vs
-- revisão-e-assinatura não estava sendo usada na prática; a UI passa a mostrar apenas
-- "responsável" na tela de detalhe e no preview lateral. Merge policy: se ambas as
-- colunas estão preenchidas, PREVALECE o conductor (quem estava executando).
--
-- Passo 1 — renomeia conductor_user_id → assignee_user_id (preserva o vínculo mais
-- ativo, sem re-atribuir; o índice sparse é renomeado junto).
ALTER TABLE intimation RENAME COLUMN conductor_user_id TO assignee_user_id;
ALTER INDEX intimation_conductor_user_id_idx RENAME TO intimation_assignee_user_id_idx;

-- Passo 2 — para linhas sem conductor mas com reviewer, promove o reviewer a
-- assignee (não perder atribuição existente antes de dropar a coluna).
UPDATE intimation
   SET assignee_user_id = reviewer_user_id
 WHERE assignee_user_id IS NULL
   AND reviewer_user_id IS NOT NULL;

-- Passo 3 — dropa reviewer_user_id (e seu índice sparse).
DROP INDEX IF EXISTS intimation_reviewer_user_id_idx;
ALTER TABLE intimation DROP COLUMN reviewer_user_id;
