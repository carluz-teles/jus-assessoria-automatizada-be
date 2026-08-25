-- Simplifica o enum de tone da draft: os rótulos hifenizados eram longos e o
-- mockup Claude Design usa 3 opções curtas (Técnico / Objetivo / Enfático).
-- IMPORTANTE: droparmos o CHECK antigo ANTES do backfill; os novos valores
-- ('tecnico', 'objetivo', 'enfatico') seriam rejeitados pelo constraint velho.

-- 1) Dropa o CHECK antigo pra liberar o backfill.
ALTER TABLE draft DROP CONSTRAINT draft_tone_check;

-- 2) Backfill: os drafts existentes usam os rótulos velhos.
UPDATE draft SET tone = 'tecnico'  WHERE tone = 'tecnico-formal';
UPDATE draft SET tone = 'objetivo' WHERE tone = 'direto-assertivo';
UPDATE draft SET tone = 'enfatico' WHERE tone = 'conciliador-institucional';

-- 3) Aplica o CHECK novo com os rótulos curtos.
ALTER TABLE draft ADD CONSTRAINT draft_tone_check
    CHECK (tone IN ('tecnico', 'objetivo', 'enfatico'));

-- 4) Atualiza o default do enum pro novo valor.
ALTER TABLE draft ALTER COLUMN tone SET DEFAULT 'tecnico';
