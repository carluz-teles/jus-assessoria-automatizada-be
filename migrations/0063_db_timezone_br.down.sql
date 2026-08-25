-- Reverso da 0065_db_timezone_br: volta pro default (UTC no cluster Postgres).
-- Só use em rollback intencional — o efeito reintroduz o bug de "1 dia em
-- atraso" após 21h BRT nas queries de prazo (documentado no up).

DO $$
BEGIN
  EXECUTE format('ALTER DATABASE %I RESET timezone', current_database());
END
$$;
