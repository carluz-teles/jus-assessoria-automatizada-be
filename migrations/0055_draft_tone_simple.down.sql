ALTER TABLE draft DROP CONSTRAINT draft_tone_check;

UPDATE draft SET tone = 'tecnico-formal'            WHERE tone = 'tecnico';
UPDATE draft SET tone = 'direto-assertivo'          WHERE tone = 'objetivo';
UPDATE draft SET tone = 'conciliador-institucional' WHERE tone = 'enfatico';

ALTER TABLE draft ADD CONSTRAINT draft_tone_check
    CHECK (tone IN ('tecnico-formal', 'direto-assertivo', 'conciliador-institucional'));

ALTER TABLE draft ALTER COLUMN tone SET DEFAULT 'tecnico-formal';
