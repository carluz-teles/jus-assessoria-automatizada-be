-- 0043_draft_attachment — join table peça ↔ documento (Peticionamento Fatia 2).
--
-- A peça (draft) can have one or more uploaded documents attached as exhibits.
-- The join table draft_attachment records the link, the category label the
-- advogado assigns, and the display order within the draft.
--
-- Business rules enforced by the DB:
--   * A document can only be attached ONCE per draft (UNIQUE draft_id + document_id).
--   * Deleting the draft cascades the attachment rows (ON DELETE CASCADE on draft_id).
--   * The document itself is NEVER deleted here (ON DELETE RESTRICT on document_id) —
--     the document lifecycle is owned by the document slice.
--   * category is validated by a CHECK constraint (same closed set as the Go layer).
--   * tenant_id is inherited from the draft/document it links and is used for the
--     two-barrier isolation (app filter + RLS).

CREATE TABLE draft_attachment (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   uuid NOT NULL REFERENCES tenant(id),
    draft_id    uuid NOT NULL REFERENCES draft(id) ON DELETE CASCADE,
    document_id uuid NOT NULL REFERENCES document(id) ON DELETE RESTRICT,
    category    text NOT NULL DEFAULT 'Outro'
                    CHECK (category IN (
                        'Procuração',
                        'Comprovante de endereço',
                        'Contrato',
                        'Provas documentais',
                        'Declaração de hipossuficiência',
                        'Outro'
                    )),
    position    int  NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),

    UNIQUE (draft_id, document_id)
);

-- Supports the read model order (position ASC, created_at ASC) and the FK lookup
-- when validating that a draft_id belongs to the right tenant.
CREATE INDEX draft_attachment_draft_id_idx    ON draft_attachment (draft_id);
CREATE INDEX draft_attachment_document_id_idx ON draft_attachment (document_id);
CREATE INDEX draft_attachment_tenant_draft_idx ON draft_attachment (tenant_id, draft_id);

-- Row-Level Security — barrier 2. Identical pattern to every other tenant-scoped table.
ALTER TABLE draft_attachment ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON draft_attachment
    USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
