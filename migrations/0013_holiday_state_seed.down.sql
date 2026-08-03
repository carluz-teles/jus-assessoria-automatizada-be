-- 0013_holiday_state_seed.down.sql — remove o seed de feriados estaduais.
-- Só as linhas STATE; NATIONAL (runtime/BrasilAPI) e COURT (futuro) intactas.

DELETE FROM holiday WHERE scope = 'STATE';
