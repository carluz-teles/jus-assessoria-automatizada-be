-- 0040_court_record_ai_resume — persists the AI-generated process summary on the
-- court_record row itself, so GET /v1/processos/:id/resume stops regenerating it on
-- every open (sync-on-first-GET, write-once, serve-from-cache thereafter).
--
-- Trigger (decisão travada): sync-on-first-GET, persist-on-success, serve-from-cache
-- thereafter. No default, no backfill — existing court_records simply have NULL until
-- their next GET, at which point the use case (internal/acquisition/resumo.go) calls
-- the LLM once and writes the result through. No invalidation logic in v0: write-once
-- by design (a re-open of an already-summarized court_record NEVER calls the LLM
-- again for the summary). The generated_at timestamp in the response informs the user
-- of the summary's age.
--
-- SCHEMA ONLY — zero slice logic (that lives in internal/acquisition). RLS: court_record
-- already carries tenant_id + the tenant_isolation policy (0001) — no isolation work
-- needed here.

ALTER TABLE court_record
    ADD COLUMN ai_resume              jsonb,
    ADD COLUMN ai_resume_generated_at timestamptz;