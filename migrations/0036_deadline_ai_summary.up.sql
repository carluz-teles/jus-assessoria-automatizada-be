-- 0036_deadline_ai_summary — persists the AI-generated "O que aconteceu"/"O que fazer"
-- summary on the deadline row itself, so GET /v1/prazos/:id/suggested-tasks stops
-- regenerating it on every open (fixação do texto exibido; the suggested TASKS keep being
-- generated fresh on every call — only the summary/recommendation freeze).
--
-- Trigger (decisão travada): sync-on-first-GET, persist-on-success, serve-from-cache
-- thereafter. No default, no backfill — existing prazos simply have NULL until their next
-- GET, at which point the use case (internal/deadline/suggest.go) calls the LLM once and
-- writes the result through. No invalidation logic: write-once, by design (a re-open of an
-- already-summarized prazo NEVER calls the LLM again for the summary).
--
-- SCHEMA ONLY — zero slice logic (that lives in internal/deadline). RLS: deadline already
-- carries tenant_id + the tenant_isolation policy (0001) — no isolation work needed here.

ALTER TABLE deadline
    ADD COLUMN ai_summary                text,
    ADD COLUMN ai_recommendation         text,
    ADD COLUMN ai_summary_generated_at   timestamptz;
