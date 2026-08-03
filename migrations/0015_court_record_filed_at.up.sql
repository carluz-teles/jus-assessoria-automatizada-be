-- 0015_court_record_filed_at — o enriquecimento DATAJUD revela a data de
-- ajuizamento (dataAjuizamento), o único campo do court_record que a fonte de
-- descoberta (DJEN) nunca carrega. judging_body já entrou na 0014; filed_at fecha
-- o par documentado em docs/erd-modelo-de-dados.md. Nullable: um record descoberto
-- pelo DJEN fica NULL até o DATAJUD enriquecê-lo.
ALTER TABLE court_record ADD COLUMN filed_at date;
