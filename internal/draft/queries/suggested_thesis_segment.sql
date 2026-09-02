-- suggested_thesis_segment queries (thesis↔trecho da peça, migration 0095).
-- Draft-scoped. Escritas rodam na tx da geração (RLS scope + filtro explícito de
-- tenant_id como barreira 1).

-- name: InsertSuggestedThesisSegment :one
-- Persiste um segmento (trecho da peça) de uma tese, escrito na MESMA tx da
-- geração, logo após o HTML ser produzido e casado com a tese por heading.
INSERT INTO suggested_thesis_segment (
    suggested_thesis_id, tenant_id, draft_id, heading, conteudo, position, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    now()
)
RETURNING *;

-- name: ListSuggestedThesisSegmentsByDraft :many
-- Todos os segmentos das teses de um draft em UMA query (evita N+1): JOIN
-- suggested_thesis pra o caller agrupar por suggested_thesis_id em memória.
-- Ordenado por posição da tese, depois posição do segmento. Scoped por
-- (draft_id, tenant_id).
SELECT s.* FROM suggested_thesis_segment s
JOIN suggested_thesis t ON t.id = s.suggested_thesis_id
WHERE t.draft_id = $1 AND s.tenant_id = $2
ORDER BY t.position, s.position, s.created_at;

-- name: DeleteSuggestedThesisSegmentsByDraft :exec
-- Limpa os segmentos de um draft antes de re-gerar a peça (regeração sempre
-- reescreve). Scoped por (draft_id, tenant_id) — coluna direta, sem JOIN.
DELETE FROM suggested_thesis_segment
WHERE draft_id = $1 AND tenant_id = $2;
