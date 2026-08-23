-- Peticionamento Fatia 5 — Teses/Tom/Instruções: persist the Gerar-time generation
-- parameters on the draft row so the async worker (which reloads the draft) can
-- read them without changing the draft.generation_requested event payload.
--
-- tone: closed set, defaults to "tecnico-formal" (the historical prompt wording —
-- existing drafts and callers that omit tone keep identical behavior).
-- instructions: free-text advogado guidance, capped at 2000 chars (same bound as
-- ChatRequest.Question).
-- selected_theses: the tese labels the advogado picked from /theses to steer Gerar;
-- text[] because a thesis reference is a plain string, not a structured citation.

ALTER TABLE draft
    ADD COLUMN tone            text NOT NULL DEFAULT 'tecnico-formal',
    ADD COLUMN instructions    text,
    ADD COLUMN selected_theses text[] NOT NULL DEFAULT '{}';

ALTER TABLE draft
    ADD CONSTRAINT draft_tone_check
    CHECK (tone IN ('tecnico-formal', 'direto-assertivo', 'conciliador-institucional'));

ALTER TABLE draft
    ADD CONSTRAINT draft_instructions_length_check
    CHECK (instructions IS NULL OR length(instructions) <= 2000);
