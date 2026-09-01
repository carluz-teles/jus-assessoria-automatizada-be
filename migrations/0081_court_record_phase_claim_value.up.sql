-- Fase processual e valor da causa no court_record, pra fechar o header do cockpit.
--
-- phase: fase DERIVADA (Conhecimento→Instrução→Sentença→Recurso→Execução) a partir da
--   classe + dos movimentos CNJ (tpu_code) no grade do DATAJUD — ver
--   internal/acquisition/fase.go. NULL até o primeiro grade.
-- phase_override: correção MANUAL do advogado; quando presente, vence a derivada
--   (a fase efetiva do read model é COALESCE(phase_override, phase)).
-- claim_value: valor da causa. NÃO tem fonte automática (nem eproc nem DATAJUD o
--   expõem — ver lib/eproc.parseProcessHTML e migration 0076); é preenchido À MÃO.
ALTER TABLE court_record
  ADD COLUMN phase          text,
  ADD COLUMN phase_override text,
  ADD COLUMN claim_value    numeric(15,2);

ALTER TABLE court_record
  ADD CONSTRAINT court_record_phase_check
    CHECK (phase IS NULL OR phase IN ('CONHECIMENTO','INSTRUCAO','SENTENCA','RECURSO','EXECUCAO')),
  ADD CONSTRAINT court_record_phase_override_check
    CHECK (phase_override IS NULL OR phase_override IN ('CONHECIMENTO','INSTRUCAO','SENTENCA','RECURSO','EXECUCAO'));
