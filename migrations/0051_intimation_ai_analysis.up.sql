-- 0051_intimation_ai_analysis — persists the AI-generated analysis of one intimation on
-- the intimation row itself, backing POST /v1/intimacoes/:id/analise ("Analisar esta
-- intimação"): the IA reads the teor (intimation.content) and produces a plain-language
-- "O que aconteceu" (ai_summary) plus the "Providências derivadas" (ai_providencias, a
-- jsonb array of {title, description}). ai_analyzed_at stamps when the analysis last ran.
--
-- Unlike court_record.ai_resume (write-once, sync-on-first-GET), this analysis is
-- RE-EXECUTABLE: the FE "Gerar novamente" re-runs POST and OVERWRITES all three columns.
-- NULL ai_analyzed_at = never analysed (the FE renders the pré-análise CTA); non-NULL =
-- pós-análise (the FE renders the summary + providências). Degraded mode (LLM off/erro)
-- persists ai_summary='' + ai_providencias='[]' with a fresh ai_analyzed_at, so the FE
-- distinguishes "analysed but IA unavailable" from "not analysed yet".
--
-- SCHEMA ONLY — zero slice logic (that lives in internal/acquisition). RLS: intimation
-- already carries tenant_id + the tenant_isolation policy (0001) — no isolation work here.

ALTER TABLE intimation
    ADD COLUMN ai_summary      text,
    ADD COLUMN ai_providencias jsonb,
    ADD COLUMN ai_analyzed_at  timestamptz;
