# Observabilidade do fluxo de IMPORT — New Relic

Guia de painel e alertas do fluxo de import (aquisição), construído sobre a instrumentação
que vive no código. **Fonte de verdade dos nomes:** `lib/telemetry/sampler.go` (sampling),
`internal/acquisition/metrics.go` (métricas do funil), `lib/events/queue_metrics.go`
(profundidade de fila), e os spans em `djen.go`/`datajud.go`/`sync.go`/`enrichment.go`.

Estas queries são NRQL prontas pra colar em widgets/alertas. Um `import-dashboard.json`
importável acompanha, já com os `accountIds` setados para a conta **8336829** — o New
Relic **não** reescreve o account id no import, então pra outra conta troque o número
antes de importar.

## Contexto: por que só o import aparece

O trace é **deny-by-default** (`OTEL_TRACES_MODE=import-only`, o padrão): só o fluxo de
import gera trace no backend — o resto (preflight, polling, reads) é dropado no sampler.
**Logs e métricas NÃO são afetados por sampling** (todo log e toda métrica chegam), então:

- **Trace** → só import (POST /v1/acquisition/integrations, `acquisition.* process`,
  `scheduler *`, e os filhos `djen.*`/`datajud.*`). Cada evento é um **trace próprio
  linkado** ao produtor — um backfill grande vira milhares de traces pequenos.
- **Erros de qualquer rota** → seguem visíveis via `Log` (com `trace.id`) e métricas RED.
- **Incidente:** `OTEL_TRACES_MODE=all` reabre tudo, via Terraform (`infra/terraform/main.tf`),
  nunca `railway variables --set` direto.

---

## 1. Funil de import (métricas — `FROM Metric`)

Contadores aditivos no ponto de trabalho (`internal/acquisition/metrics.go`). OTLP cumulativo
→ use `sum()`; o New Relic entrega o incremento por bucket.

| Widget | NRQL |
|---|---|
| Processos descobertos (novos vs revistos) | `FROM Metric SELECT sum(import.court_records) FACET state TIMESERIES` |
| Andamentos novos landados | `FROM Metric SELECT sum(import.docket_entries_new) TIMESERIES` |
| Intimações landadas (novas vs revistas) | `FROM Metric SELECT sum(import.intimations) FACET state TIMESERIES` |
| Enriquecimentos DATAJUD aplicados | `FROM Metric SELECT sum(import.enrichments_applied) TIMESERIES` |
| Diário nacional landado (ingestão) | `FROM Metric SELECT sum(import.diario_publications) TIMESERIES` |
| Publicações folded em tenants (match) | `FROM Metric SELECT sum(import.match_publications) TIMESERIES` |
| Resumo do dia (billboard) | `FROM Metric SELECT sum(import.court_records) AS 'processos', sum(import.intimations) AS 'intimações', sum(import.diario_publications) AS 'diário' SINCE 1 day ago` |

## 2. Chamadas externas DJEN/DATAJUD (métrica + span)

O sinal de rate-block do DJEN (o problema recorrente) e a latência das chamadas.

| Widget | NRQL |
|---|---|
| Mix de status DJEN | `FROM Metric SELECT sum(import.djen_requests) FACET status_class TIMESERIES` |
| **Taxa de 429 DJEN** (rate-block) | `FROM Metric SELECT sum(import.djen_requests) WHERE status_class = '429' TIMESERIES` |
| Latência DJEN request p50/p95/p99 | `FROM Span SELECT percentile(duration.ms, 50, 95, 99) WHERE name = 'djen.request_page' TIMESERIES` |
| Cooldown / retry-after visto | `FROM Span SELECT max(djen.retry_after_ms) WHERE djen.rate_limited IS TRUE TIMESERIES` |
| Latência DATAJUD p95 | `FROM Span SELECT percentile(duration.ms, 95) WHERE name = 'datajud.fetch_by_number' TIMESERIES` |
| Status DATAJUD | `FROM Span SELECT count(*) WHERE name = 'datajud.fetch_by_number' FACET http.response.status_code TIMESERIES` |

## 3. Filas asynq (gauge — `latest`)

`asynq.queue.depth` (registrado no relay, `lib/events/queue_metrics.go`), por fila e estado.

| Widget | NRQL |
|---|---|
| Backlog pendente por fila | `FROM Metric SELECT latest(asynq.queue.depth) WHERE state = 'pending' FACET queue TIMESERIES` |
| Estado completo (área empilhada) | `FROM Metric SELECT latest(asynq.queue.depth) FACET queue, state TIMESERIES` |
| Tarefas arquivadas (DLQ) | `FROM Metric SELECT latest(asynq.queue.depth) WHERE state = 'archived' FACET queue TIMESERIES` |

## 4. Erros do import (span — `error.kind`)

Todo span de falha carrega `error.kind` (via `obs.Record`, de `AppError`). Um 429 do DJEN
(`SERVICE_UNAVAILABLE`) lê apartado de um bug (`INFRA_ERROR`).

| Widget | NRQL |
|---|---|
| Falhas por kind e span | `FROM Span SELECT count(*) WHERE error.kind IS NOT NULL AND (name LIKE 'acquisition.%' OR name LIKE 'djen.%' OR name LIKE 'datajud.%' OR name LIKE 'scheduler %') FACET error.kind, name TIMESERIES` |
| Taxa de erro dos consumers de import | `FROM Span SELECT percentage(count(*), WHERE otel.status_code = 'ERROR') WHERE name LIKE 'acquisition.%process' TIMESERIES` |
| Backfills disparados (por trace) | `FROM Span SELECT count(*) WHERE name = '/v1/acquisition/integrations' TIMESERIES` |

## 5. Runtime Go (`FROM Metric`, `go.*`)

De `runtime.Start` no `telemetry.Setup` (todos os binários; facet por `service.name`).

| Widget | NRQL |
|---|---|
| Goroutines por serviço | `FROM Metric SELECT latest(go.goroutine.count) FACET service.name TIMESERIES` |
| Memória usada | `FROM Metric SELECT latest(go.memory.used) FACET service.name TIMESERIES` |
| Latência de scheduling (p95) | `FROM Metric SELECT percentile(go.schedule.duration, 95) FACET service.name TIMESERIES` |

> **Nomes de métrica exatos:** a ingestão OTLP→NR pode variar o naming. Descubra o que
> chegou com:
> `FROM Metric SELECT uniques(metricName) WHERE metricName LIKE 'go.%' OR metricName LIKE 'import.%' OR metricName LIKE 'asynq.%'`

---

## Alertas (NRQL conditions)

| Alerta | NRQL | Threshold sugerido |
|---|---|---|
| **DJEN em rate-block** | `FROM Metric SELECT sum(import.djen_requests) WHERE status_class = '429'` | > 20 em 5 min (o WAF/JA3 voltou a barrar — ver `djen-throttle-is-ja3`) |
| **Backlog de fila crescendo** | `FROM Metric SELECT latest(asynq.queue.depth) WHERE state = 'pending' FACET queue` | > 5000 por 10 min (consumers não dão conta) |
| **DLQ acumulando** | `FROM Metric SELECT latest(asynq.queue.depth) WHERE state = 'archived' FACET queue` | > 100 (tarefas mortas — investigar) |
| **Falha de import (bug)** | `FROM Span SELECT count(*) WHERE error.kind = 'INFRA_ERROR' AND name LIKE 'acquisition.%'` | > 10 em 5 min |
| **DATAJUD degradado** | `FROM Span SELECT count(*) WHERE name = 'datajud.fetch_by_number' AND otel.status_code = 'ERROR'` | > 20 em 5 min |
| **Diário não ingeriu (dead-man)** | `FROM Metric SELECT sum(import.diario_publications)` | < 1 em 6h (janela útil) — o tick diário do scheduler parou |

> O dead-man (`import.diario_publications` = 0) só faz sentido com `INGESTION_ENABLED=true`.
> Configure a condition como **loss of signal** / abaixo de 1 na janela.

---

## Como aplicar

1. **Importar o dashboard:** New Relic → Dashboards → Import dashboard → cole
   `import-dashboard.json`. Já vem com `accountIds: 8336829`; o editor de import valida os
   account ids (um id inválido como `0` é rejeitado ali) — pra outra conta, troque antes.
2. **Ou montar à mão:** crie um dashboard e cole as NRQL das tabelas acima nos widgets.
3. **Alertas:** Alerts → NRQL condition → cole a query + threshold da tabela.
4. Se um widget vier vazio, rode a query de descoberta (§5) — o naming OTLP→NR do seu
   endpoint pode diferir levemente (ex.: prefixo, `.` vs `_`).
