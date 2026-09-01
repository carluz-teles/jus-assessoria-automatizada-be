-- name: InsertDraftSegment :one
INSERT INTO draft_segment (id, tenant_id, draft_id, thesis_id, profile_section_id, conteudo, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, tenant_id, draft_id, thesis_id, profile_section_id, conteudo, created_at;
