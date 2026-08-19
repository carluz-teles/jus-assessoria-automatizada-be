-- ============================================================================
-- Reconciliação ONE-OFF de prazos: MISSED/OPEN → MET quando já há movimento de
-- resposta nos autos (petição/manifestação/contestação/…) em/após o start_date.
--
-- NÃO É MIGRAÇÃO. Este arquivo NÃO vive em /migrations/ e NÃO roda no boot do api.
-- É um script manual, rodado UMA VEZ por um operador, com backup/snapshot antes.
--
-- POR QUÊ: muitos prazos históricos nasceram já vencidos no backfill de onboarding
-- (a intimação era antiga, a carência D+1 já havia passado no momento da derivação),
-- então foram marcados MISSED — mesmo quando o processo JÁ TINHA a peça de resposta
-- nos andamentos (docket_entry). Este UPDATE ressuscita esses prazos para MET. É o
-- mesmo predicado do caminho LIVE (internal/deadline: OnDocketEntryObserved →
-- HasResponseMovement), aplicado em massa aos ~14k prazos MISSED/OPEN já existentes.
--
-- ESCOPO (travado): SÓ status IN ('MISSED','OPEN'). NUNCA PENDING (sugestão não
-- confirmada), NUNCA CANCELLED (revogado), NUNCA um MET já reconciliado. Reusa o
-- status MET existente — SEM estado novo, SEM coluna nova, SEM migração de schema.
--
-- SILENCIOSO: este script NÃO emite deadline.met (não escreve no outbox). É deliberado
-- — reconciliar 14k prazos de uma vez dispararia um flood de notificações "prazo
-- cumprido" sobre fatos históricos. Os read models derivam o status DIRETO da coluna
-- deadline.status, então a tela reflete o MET assim que o UPDATE commita, sem evento.
-- (O caminho LIVE, ao contrário, emite deadline.met por cumprimento real e recente.)
--
-- ROLLBACK: NÃO há coluna de auditoria (reconciled_at) por decisão de escopo, então
-- este UPDATE é IRREVERSÍVEL por si só — não há como distinguir, depois, um MET
-- reconciliado de um MET legítimo. O rollback é RESTAURAR O SNAPSHOT tirado antes de
-- rodar. TIRE O SNAPSHOT. Rode em STAGING primeiro, com EXPLAIN ANALYZE (abaixo) para
-- dimensionar o custo, e só então em produção.
--
-- ISOLAMENTO DE TENANT: o filtro cr.tenant_id = d.tenant_id no EXISTS mantém o
-- movimento escopado ao mesmo tenant do prazo (docket_entry não tem tenant_id próprio;
-- ele o herda via court_record). Rode como um papel que enxerga todos os tenants
-- (bypass de RLS), pois é uma operação administrativa cross-tenant.
--
-- PREDICADO (idêntico ao HasResponseMovement do slice):
--   um movimento é RESPOSTA quando seu tpu_code ∈ (85,118,383,433,235)  -- peças TPU
--   OU (sem tpu_code) quando o texto casa o regex de tipos de peça de resposta;
--   e ocorreu em/após o start_date do prazo (uma peça anterior não o cumpre).
-- ============================================================================

-- ── 1. DRY-RUN: quantos prazos seriam reconciliados, por tenant. Rode ANTES. ──
-- SELECT d.tenant_id, count(*) AS would_reconcile
-- FROM deadline d
-- WHERE d.status IN ('MISSED','OPEN')
--   AND EXISTS (
--     SELECT 1 FROM docket_entry de
--     JOIN court_record cr ON cr.id = de.court_record_id
--     WHERE cr.id = d.court_record_id
--       AND cr.tenant_id = d.tenant_id
--       AND ( de.tpu_code IN (85,118,383,433,235)
--             OR (de.tpu_code IS NULL AND de.text ~* 'petiç|manifest|contestaç|impugnaç|recurso|embargos|defesa') )
--       AND de.occurred_at >= d.start_date
--   )
-- GROUP BY d.tenant_id
-- ORDER BY would_reconcile DESC;

-- ── 2. EXPLAIN ANALYZE do UPDATE em STAGING (dimensiona o custo antes da prod). ──
-- EXPLAIN ANALYZE
-- UPDATE deadline d
-- SET status = 'MET'
-- WHERE d.status IN ('MISSED','OPEN')
--   AND EXISTS ( ... o mesmo EXISTS do UPDATE abaixo ... );

-- ── 3. O UPDATE. Rode dentro de uma transação; revise o rowcount antes do COMMIT. ──
BEGIN;

UPDATE deadline d
SET status = 'MET'
WHERE d.status IN ('MISSED', 'OPEN')
  AND EXISTS (
    SELECT 1
    FROM docket_entry de
    JOIN court_record cr ON cr.id = de.court_record_id
    WHERE cr.id = d.court_record_id
      AND cr.tenant_id = d.tenant_id
      AND de.occurred_at >= d.start_date
      AND (
        de.tpu_code IN (85, 118, 383, 433, 235)
        OR (
          de.tpu_code IS NULL
          AND de.text ~* 'petiç|manifest|contestaç|impugnaç|recurso|embargos|defesa'
        )
      )
  );

-- Confira o rowcount reportado contra o dry-run do passo 1. Se bater, COMMIT; senão ROLLBACK.
COMMIT;
