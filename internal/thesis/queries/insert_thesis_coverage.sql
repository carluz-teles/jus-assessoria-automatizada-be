-- name: InsertThesisCoverage :one
INSERT INTO thesis_coverage (id, thesis_id, resultado, detalhe, created_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (thesis_id) DO UPDATE
SET resultado = EXCLUDED.resultado, detalhe = EXCLUDED.detalhe, created_at = EXCLUDED.created_at
RETURNING id, thesis_id, resultado, detalhe, created_at;
