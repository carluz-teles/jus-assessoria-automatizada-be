-- rótulo manual do processo (court_case.label) — o TÍTULO que o advogado escolhe pra
-- diferenciar o caso na lista/painel/cockpit (Achado 1: classe·assunto sozinho repete em
-- 62-74% dos processos). Escrito via PATCH /v1/processos/:id (UpdateProcessoManualRequest.
-- Label), reusando o MESMO hop court_record → court_case de AssignResponsible
-- (responsible.sql's GetCaseIDByCourtRecord) — não há query nova de resolve aqui.

-- name: UpdateCaseLabel :exec
-- Grava (ou limpa, quando o argumento vem NULL) o court_case.label, tenant-scoped
-- (barreira 1 + RLS barreira 2). Direto em court_case, SEM cascata — o label é uma
-- propriedade do CASE (compartilhada entre graus), nunca da intimação/court_record.
-- O caseID já foi resolvido a partir do court_record chamador pela query
-- GetCaseIDByCourtRecord (responsible.sql) — mesmo padrão de AssignResponsible.
UPDATE court_case
   SET label = sqlc.narg('label')::text
 WHERE id = @case_id::uuid AND tenant_id = @tenant_id::uuid;
