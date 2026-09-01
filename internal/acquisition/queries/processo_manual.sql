-- name: UpdateProcessoManualFields :exec
-- Campos do processo preenchidos À MÃO no cockpit: a fase (phase_override — vence a
-- derivada no read model) e o valor da causa (claim_value, sem fonte automática). PATCH
-- parcial: cada campo só é escrito quando o argumento vem não-nulo (COALESCE mantém o
-- valor atual quando o cliente não mandou aquele campo). Tenant-scoped (barreira 1 + RLS).
UPDATE court_record
   SET phase_override = COALESCE(sqlc.narg('phase_override')::text, phase_override),
       claim_value    = COALESCE(sqlc.narg('claim_value')::numeric, claim_value)
 WHERE id = @court_record_id::uuid AND tenant_id = @tenant_id::uuid;
