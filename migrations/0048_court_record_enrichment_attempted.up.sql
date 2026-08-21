-- 0048_court_record_enrichment_attempted — dá ao enriquecimento DATAJUD uma marca
-- de VARREDURA por (tenant, tribunal). O redesign troca o enriquecimento de "1
-- court_record_observed → 1 fetch rate-limited (120/min)" por um job EM LOTE por
-- (tenant, tribunal): ele varre os court_records ainda não enriquecidos daquele
-- tribunal, busca-os em UM request _search por lote (terms) e grada todos. Para não
-- re-varrer para sempre um processo que o DATAJUD não indexa (ou não gradua), cada
-- registro visitado por um lote é MARCADO — enrichment_attempted_at — visto ou não.
-- A marca (não o completeness) é o que fecha a varredura de forma determinística.
ALTER TABLE court_record ADD COLUMN enrichment_attempted_at timestamptz;

-- Índice parcial de VARREDURA do lote: só as linhas que ainda pedem enriquecimento —
-- completeness abaixo do teto DATAJUD (0.9), nunca tentadas, e não superseded (o
-- placeholder aposentado no merge do enriquecimento fica <0.9 mas é lixo estrutural;
-- excluí-lo evita re-varrer para sempre). Serve SelectDueForEnrichment (por tenant+
-- court) e CountRemainingDueForJob (por tenant, todos os tribunais da importação). O
-- predicado é o mesmo dos dois SELECTs, então o planner usa o índice; ele encolhe à
-- medida que os lotes marcam os registros, então a varredura fica barata até esvaziar.
CREATE INDEX court_record_enrichment_scan_idx
  ON court_record (tenant_id, court)
  WHERE completeness < 0.9 AND enrichment_attempted_at IS NULL AND lifecycle <> 'SUPERSEDED';
