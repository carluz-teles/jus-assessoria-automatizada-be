-- Metadados da capa do processo capturados no fetch dos autos (eproc), guardados no
-- court_record para não exigir backfill quando uma fatia futura precisar deles. A
-- classe (class), o órgão julgador (judging_body) e a data de autuação (filed_at) já
-- existem e são enriquecidos no mesmo passo; estas três são específicas do eproc e
-- ainda não tinham coluna. O valor da causa NÃO entra: a página do eproc não o expõe
-- (ver lib/eproc.parseProcessHTML) — por isso o 0076 o dropou e ele segue fora.
ALTER TABLE court_record
  ADD COLUMN magistrate      text,  -- magistrado responsável (#txtMagistrado)
  ADD COLUMN court_situation text,  -- situação no eproc, ex. "MOVIMENTO" (#txtSituacao)
  ADD COLUMN competence      text;  -- competência, ex. "Juizado Especial Cível" (#txtCompetencia)
