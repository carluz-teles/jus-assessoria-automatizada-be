# PM Handoff — Motor de Prazos V1

## OBJECTIVE
Extend the existing deadline (`deadline`) slice from V0 to V1, adding precedência de fontes, selo de confiança (ortogonal ao estado), validação cruzada, e política por tenant com piso inegociável. O prazo deve nascer ATIVO imediatamente ao cálculo, sem esperar confirmação humana. IA só atua na classificação do tipo de ato (no ingest, para intimações omissas), nunca no cálculo determinístico.

## USER_GOAL
Advogados e escritórios têm prazo sempre materializado ao importar uma intimação — nem "prazo cego". O sistema sinaliza quando um humano precisa revisar a data (selo `a_apurar`), mas o relógio conta desde o ingest. O escritório define sua política de confirmação (seletiva padrão, ou estrita), mas o piso fixo garante que IA e divergência sempre exigem humano.

## REQUIREMENTS

### Functional (from V1 design)

| # | Req | Description |
|---|-----|-------------|
| F1 | **Precedência de fontes** | Todo prazo carrega `origem` (declarado \| validado \| calculado \| divergente \| ia \| manual). Fonte primária = `prazo_declarado` da intimação. Se não houver declarado, IA classifica o tipo → tabela legal → data. |
| F2 | **Selo de confiança (ortogonal)** | Cada prazo tem `selo` (CONFIANÇA \| A_APURAR). O selo é independente do estado operacional (ativo/metido/cancelado). Padrão: sistema assume CONFIANÇA quando declarado + calculado batem. Divergente ou tipo IA → A_APURAR. |
| F3 | **Confirmação exigida por política** | `confirmacao_exigida` flag, regida por `deadline_policy` do tenant. Modo seletivo (default): sistema assume CONFIANÇA, confirmação opcional em lote. Modo estrita: todo prazo `a_apurar` vai para confirmação humana. O piso nunca reduz abaixo de `a_apurar` para casos de IA/divergência. |
| F4 | **Validação cruzada** | Quando há `prazo_declarado` E data calculada, compara os dois. Convergente → alta confiança. Divergente → persiste em `cross_validation` com a decisão humana (aceita_declarado \| aceita_calculado \| ajuste_manual). |
| F5 | **IA apenas na classificação (ingest)** | O único ponto de IA é classificar o tipo de ato quando a intimação é omissa (sem `prazo_declarado`). Saída: tipo de ato + confiança. NUNCA uma data. O fallback de prazo roda ansioso no ingest — toda intimação nasce com prazo (declared → confiavel; omissa → a_apurar via IA). |
| F6 | **Ativo desde o cálculo** | O prazo nasce ATIVO (`OPEN` status equivalent) imediatamente após o cálculo. O relógio conta, agenda de alerta dispara, nem espera confirmação. O selo (`confiavel`/`a_apurar`) é dimensão ortogonal. |
| F7 | **Policy per tenant** | `deadline_policy` table: `confirmacao_obrigatoria` (false=seletiva default, true=estrita). A política só aumenta o rigor, nunca reduz abaixo do piso (IA/divergência sempre exigem humano). |
| F8 | **Snapshot de feriados aplicados** | `calc_memory` persiste `prazo_base`, `termo_inicial_regra`, `dobra_motivo`, `dias_uteis`, `tabela_legal_ref`, `ia_tipo_inferido`, `ia_confianca`, `calendar_provider_version`. Proveniência — o cálculo de hoje deve continuar explicável amanhã. |
| F9 | **Recálculo aditivo** | Movimento superveniente (`docket_entry_observado`) nunca sobrescreve; gera novo `deadline_event` e nova deriva. Memória anterior preservada na trilha. |

### Non-Functional
- **Idempotência**: `OnIntimationObserved` deve ser idempotente por `(notification_id, providencia)` — reprocessar não duplica prazos.
- **RLS + tenant_id**: Duas barreiras: filtro na aplicação + `SET LOCAL app.tenant_id` no Postgres. `deadline_policy` é por tenant.
- **Observabilidade**: Métricas OTEL — zero perdas de prazo, % automático vs manual, taxa de fallback IA, taxa de divergência, latência do provedor de calendário, idade dos prazos `a_apurar`, tempo captura→prazo ativo.

### Technical Deliverables (what the slice must expose)

1. **entity.go** — Adicionar campos: `Origem`, `Selo`, `ConfirmacaoExigida`, `CrossValidation` (opcional), aprimorar `CalcMemory` com `IA_TipoInferido` e `IA_Confianca`. Manter mapper aware pgtype.

2. **domain.go** — Novos casos de uso:
   - `CalcularPrazo` (já existe em V0, agora com precedência de fontes + IA fallback)
   - `ClassificarTipo` (chama serviço de IA, só no ingest para intimações omissas)
   - `ValidarCruzado` (declarado × calculado → converge/divergente)
   - `AtribuirSelo` (origem → selo, aplicação da política do tenant com piso)
   - `ConfirmarPrazo` (atualiza selo + flag de confirmação exigida)
   - `ApurarPrazo` (resolve divergência/IA, padrão humano obrigatório — piso)
   - `OverridePrazo` (ajuste manual, grava novo end_date + event)
   - `RecalcularPrazo` (movimento superveniente → novo event, memória preservada)

3. **listener.go** — Consumir eventuais novos eventos se necessário (padrão atual já cobre `movimento_observado`; V1 pode precisar de `prazo.classificado` se IA classifier for separado).

4. **handler.go** — Rotas V1 já existem (`RegisterV1`). Novas rotas/ops podem ser adicionadas como extensão (confirm, ajuste, preview já cobrem grande parte; V1 pode adicionar `POST /v1/prazos/:id/validar-cruzado`, `GET /v1/prazos/:id/selo`).

5. **repository.go** — Novas queries/sqlc: `InsertDeadline` (já idempotente por notification_id), `GetDeadlineWithOrigemSelo` (para ler origem+selo juntos), `SetCrossValidation`, `ListDeadlinePolicies`. RLS já presente.

6. **validation.go** — Novos validações: `origem` no closed set, `selo` no closed set, `confirmacao_exigida` lógico (server-side, não body). Métodos `Validate()` em requests existentes podem ser estendidos.

7. **mapper.go** — Absorver novos pgtype mappings para `Origem`, `Selo`, `CrossValidation`. O padrão `derefString`/`textToNull` já existente continua adequado.

8. **events.go** — Eventos V1 (já emitidos em V0, podem ser estendidos):
   - `prazo.prazo_calculado` (já existe — pode enriquecer com `origem`)
   - `prazo.prazo_assumido` (confiável, política seletiva)
   - `prazo.confirmacao_requerida` (a apurar, ou política estrita)
   - `prazo.divergencia_detectada` (quando declared ≠ calculated)
   - `prazo.prazo_alterado` (recálculo por movimento superveniente)

9. **queries/*.sql** — Novas queries sqlc para políticas, validação cruzada, leitura de memoria de cálculo.

### Edge Cases

| # | Edge Case | Description |
|---|-----------|-------------|
| E1 | **Intimação sem prazo declarado, IA indisponível** | O prazo omissa fica `a_apurar` sem data inferida. Alerta crítico + cálculo manual exigido. Nunca chuta data. |
| E2 | **Provedor de calendário cai no ingest** | O declared segura o relógio (fallback). O omissa (precisa de calendário) fica pendente de reprocessamento quando o provedor volta. Nunca marca sem data silenciosamente. |
| E3 | **Divergente (declarado ≠ calculado)** | Persiste em `cross_validation`. O humano decide via interface: aceita_declarado / aceita_calculado / ajuste_manual. O selo fica `a_apurar` até decisão. |
| E4 | **Política estrita + confiável assumido** | A política pode levar os "confiáveis" à confirmação, mas nunca reduz abaixo do piso: IA/divergência sempre `a_apurar`. |
| E5 | **Fração alta de intimações omissas** | Se a maioria das intimações não declara prazo, o custo de IA no ingest sobe. Métrica monitorada; se doer, revisitar se fallback dessas deveria ser diferido (aceitando prazo cego até o "Analisar"). |
| E6 | **Cache do calendário vs. consulta a cada cálculo** | Consultar provedor a cada cálculo acopla latência/disponibilidade ao caminho crítico. Cache local por comarca é candidato para reduzir acoplamento. |
| E7 | **Reflexo da política no Inbox** | No modo seletivo, um prazo `confiável` não-confirmado é normal (não destaque). No modo estrito, mesmo `confiável` não-confirmado é pendência. A `deadline_policy` reconfigura o que o Inbox destaca. |
| E8 | **Recálculo retroativo de feriado** | Prazos já calculados mantêm o snapshot do `calendar_provider_version` no `calc_memory`. Correção retroativa é decisão de produto; modelo permite manter prazo existente. |

### ACCEPTANCE_CRITERIA

Each criterion must be verifiable by test or objective command.

| # | Criterion | Definition |
|---|-----------|------------|
| A1 | **Prazo declarado → CONFIÁVEL** | Quando intimação tem `prazo_declarado`, o prazo é criado com `origem=declarado`, `selo=CONFIANÇA`, status ATIVO, sem IA chamada. |
| A2 | **Prazo omitido → A_APURAR via IA** | Quando intimação sem `prazo_declarado`, IA classifica tipo → tabela legal dá o prazo. Resultado: `origem=ia`, `selo=A_APURAR`, status ATIVO, flag `confirmacao_exigida=true`. |
| A3 | **Convergência cruzada** | Quando declarado e calculado batem: `resultado=convergente` em `cross_validation` (se tabela existir), selo mantém CONFIÁVEL. |
| A4 | **Divergência persistida** | Quando declarado ≠ calculado: `cross_validation` persiste com `resultado=divergente`, `causa_provavel`, e `decisao` humana. Selo = `A_APURAR`. |
| A5 | **Piso inegociável** | Casos com `origem=ia` ou `resultado=divergente`: `confirmacao_exigida=true` independentemente da política do tenant. A política só pode abrir mais portões, nunca fechar abaixo do piso. |
| A6 | **Ativo desde o cálculo** | Após `OnIntimationObserved` completar, o prazo tem status ATIVO (equivalente OPEN) imediatamente. Agenda de alerta (reminder_check) é agendada na mesma tx. |
| A7 | **Idempotência de replay** | Reprocessar `aquisicao.movimento_observado` para a mesma intimação não cria segundo prazo (dedup por event_id). |
| A8 | **Política seletiva vs estrita no Inbox** | Modo seletivo: prazo `confiável` não-confirmado aparece como "normal". Modo estrito: mesmo `confiável` não-confirmado aparece como pendência/requer ação. |
| A9 | **Recálculo gera novo event** | Movimento superveniente (`docket_entry_observado`) dispara `recalcular` → novo `deadline_event` com tipo `recalculado`, nunca sobrescreve o existente. |
| A10 | **Snapshot de feriados** | `calc_memory.calendar_provider_version` é gravado. Query de leitura retorna a versão consultada. Se provedor mudar, prazos existentes mantêm versão antiga (snapshot). |

### AMBIGUITIES (open questions — answer changes scope)

| # | Ambiguity | Recommended default | Why important |
|---|-----------|--------------------|---------------|
| Q1 | **Fração de intimações omissas** — alta fração significa custo de IA ansioso no ingest. Se > 70% omissa, revisitar se fallback deveria ser diferido (aceitando prazo cego até o "Analisar"). | Manter fallback ansioso no ingest. Medir fração; se doer, revisitar. | Define se o time pagará IA para toda intimação no ingress ou adiará para on-demand. |
| Q2 | **Cache local do calendário** — consulta a cada cálculo acopla ao provedor; cache reduz acoplamento mas pode ficar desatualizado. | Implementar cache por comarca como optional, com invalidate pelo `calendar_provider_version`. | Trade-off entre latência crítica e controle da fonte externa. |
| Q3 | **SLA e retry do calendário** — quantas tentativas e quanto tempo antes de assumir indisponibilidade? | Retry exponencial com teto de 3 tentativas em 5min. After teto, fallback pelo declarado (se houver) ou alerta crítico para omissas. | Define o comportamento quando o provedor cai durante o ingest em massa. |
| Q4 | **Versão do provedor e recontagem retroativa** — prazos já calculados recalculam quando o provedor atualiza feriados? | Manter snapshot (`calendar_provider_version` no `calc_memory`). Recalcular só se explicitamente solicitado (não automático). | Evita mudanças retroativas inesperadas em prazos já cumpridos/atuais. |
| Q5 | **Reflexo da política no Inbox** — como a interface diferencia "não confirmado" entre seletivo e estrito? | A `deadline_policy` reconfigura o badge/label do Inbox. Seletivo: confia que o sistema assumiu. Estrito: destaca como pendência even quando confiável. | Define o trabalho de FE/UX para reconfigurar a tela de prazos. |
| Q6 | **IA classifier service** — já existe um serviço de IA no codebase ou precisa ser criado? | Verificar se `internal/advisory` ou outro conector já serve. Se não, criar serviço mínimo: `teoria → tipo_de_ato + confianca`. | Evitar duplicação de infraestrutura de IA; o V1 foca apenas na classificação no ingest. |

## NOTES for Architect & DEV

1. **Vertical slice pattern**: Each slice owns its routes (`RegisterV1`), its DB queries (sqlc), its domain logic, and its events. Add V1 changes *inside* the `internal/deadline/` slice — no need to create a new slice unless the boundary shifts.

2. **Dependency rule**: `handler|listener → domain(caso de uso) → entity ← repository(impl sqlc)`. `entity.go` never imports repo/handler/lib. `domain.go` depends on the **interface** Repository, not the impl.

3. **Changes are additive**: V1 adds new fields (origem, selo, confirmacao_exigida, cross_validation) and new use cases. V0 existing code (rule resolution, calendar math, idempotent insert) stays untouched where possible. The `rulesVersion` constant stays `"v0"` for the rule resolution layer; V1 logic sits above/following it.

4. **IA only in classification**: The only new IA call is `ClassificarTipo` no ingest, para intimações omissas. O cálculo determinístico (dias úteis/corridos, feriados) permanece puro e auditable — sem IA no loop de data.

5. **Tenancy**: `tenant_id` em toda tabela + RLS. `deadline_policy` é por tenant. O handler já lê `tenant_id` do token (Clerk org → tenant interno), nunca do body.

6. **Observabilidade**: Adicionar métricas para: `% automático vs manual`, `taxa de fallback IA`, `taxa de divergência por fonte`, `idade dos prazos a_apurar`, `tempo captura → prazo ativo`.

7. **Green gate**: `go build ./... && go test ./internal/deadline/...` deve sair 0. Testes unitários com repo mockado são o grosso; integração via docker compose.

8. **Tagging**: Após merge na `main` → tag `v1.1.0` (MINOR bump, feature nova fatia). Breaking change seria alterar contratos de API ou schema de DB incompatível → MAJOR.