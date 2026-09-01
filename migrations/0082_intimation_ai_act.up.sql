-- ai_act: o "ato principal" da intimação, classificado pela IA na análise (ex.:
-- "Contestação", "Manifestação", "Recurso"). Alimenta o TÍTULO da tela de detalhe
-- (H1 = o ato) e o pill de derivação "Ato: X". Nullable — só preenchido pós-análise;
-- pré-análise o read model/FE cai no class+subject. text (nunca enum) — é
-- classificação livre da IA, não um conjunto fechado. OVERWRITE a cada re-análise,
-- como ai_summary/ai_providencias.
ALTER TABLE intimation
    ADD COLUMN ai_act text;
