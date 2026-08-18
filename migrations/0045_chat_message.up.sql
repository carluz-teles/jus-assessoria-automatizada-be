-- 0045_chat_message — persists the multi-turn chat thread between the advogado and
-- the AI assistant for a given draft (peça). Each row is one turn: role='user' or
-- role='assistant'.
--
-- Isolation: follows the exact same pattern as the review table (0044). chat_message
-- has NO tenant_id of its own — isolation is enforced at BARRIER 1 via JOIN on
-- draft.tenant_id (every API access first tenant-guards the draft before any
-- chat_message read/write). Barrier 2 (RLS) is absent here for the same reason as
-- review: adding an RLS policy requires either a duplicate tenant_id column or a
-- policy that checks current draft ownership, adding query complexity. The barrier-1
-- guard is sufficient because no code path exposes a direct chat_message lookup by
-- id without first loading the tenant-guarded draft.
-- RECOMMENDATION: add tenant_id NOT NULL + RLS in a future slice (same as review's
-- KNOWN_RISK in migration 0044).
--
-- citations jsonb: array of {document_id, page, quote} anchoring the answer to
-- specific chunks from the case corpus. Empty array [] when the answer is not grounded.
-- grounded bool: true when the answer was generated from at least one retrieved chunk
-- AND the LLM cited at least one valid chunk. Always false for role='user' rows.
-- model_version: the model slug used for role='assistant' rows; NULL for role='user'.

CREATE TABLE chat_message (
    id             uuid        NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,
    draft_id       uuid        NOT NULL REFERENCES draft(id) ON DELETE CASCADE,
    role           text        NOT NULL CHECK (role IN ('user', 'assistant')),
    content        text        NOT NULL,
    citations      jsonb       NOT NULL DEFAULT '[]',
    grounded       boolean     NOT NULL DEFAULT false,
    model_version  text,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- Primary access pattern: load the last 50 messages of a thread ordered chronologically.
-- Also used by INSERT (append) and index supports both patterns.
CREATE INDEX chat_message_draft_created_idx ON chat_message (draft_id, created_at);
