# ERD — Execução Judicial TJSP (autos + peticionamento via portal)

> **Status:** desenho (v1 — camada de **execução judicial**: lê autos do tribunal e protocola peças).
> Depende de **Documentos/Indexing** (grounding) e de **Peças** (`draft/review/petition`) já desenhados.
> **Fonte de verdade do schema:** `erd-modelo-de-dados.md`. Onde este doc divergir do schema, o schema vence.
> **Substitui** o antigo `erd-tribunal-scraping.md` (revertido da `main`) e materializa a fatia
> "Autos / inteiro teor (COURT)" que `erd-documentos.md §4` marcou como futura/hard.
>
> Origem: consolida o `tjsp-technical-discovery.md` (discovery documental, 17/08/2026), **reconciliado**
> com o domínio existente. O discovery levantou os princípios certos; este ERD os traduz para as tabelas,
> slices e eventos que já existem — sem reinventar vocabulário.

---

## 1. Contexto & objetivo

A plataforma já **descobre e consolida** processos (DJEN/DATAJUD → `court_case/court_record/docket_entry/
intimation`), já tem **pipeline de documentos** (`document/chunk`, extração/OCR/embedding no slice
`indexing`) e já desenhou o **ciclo da peça** (`draft → review → petition`, human-in-the-loop, `erd-pecas.md`).

Falta a **ponta para fora do sistema**: acessar o processo **autenticado** no tribunal, baixar os autos
(inteiro teor) para alimentar o grounding, e — no fim do ciclo da peça — **assinar e protocolar**,
confirmando com evidência. O primeiro cliente tem a maioria dos processos no **TJSP** (e-SAJ + eproc),
então o escopo é TJSP.

**Objetivo:** um slice novo `court` que encapsula o acesso ao tribunal atrás de uma interface
**Court Provider** (um adapter por sistema: `eproc`, `e-SAJ`), expondo duas capacidades —
**READ** (fetch de autos → pipeline de documentos existente) e **WRITE** (assinatura + protocolo →
`petition`). O resto do SaaS **não conhece** Playwright, Web Signer nem MNI. Comunicação só por evento.

---

## 2. Reuse-check (Regra nº1)

O discovery propôs entidades novas (`Process`, `Event`, `Document`, `ProcessSyncJob`, `FilingJob`,
`CourtPetition`). Quase todas **já existem** com outro nome. Mapa obrigatório:

| Discovery propôs | Já existe no schema | Decisão |
|---|---|---|
| `Process` | `court_case` (a lide) + `court_record` (tramitação por grau/tribunal — "centro de gravidade") | **REUSE** — a distinção grau/tribunal já cobre parte da migração SAJ→eproc |
| `Event` (árvore de eventos do eproc) | `docket_entry` (`occurred_at`/`observed_at`, `tpu_code`, `hash`) | **REUSE** |
| `Document` (id, process_id, event_id, sha256, storage_key, ocr, mime…) | `document` (`court_record_id`, `origin=COURT`, `has_text_layer`, `storage_key`, `extractor_version`) | **EXTEND** — faltam deltas: `docket_entry_id`, `external_id`, `mime_type`, `size_bytes`, `checksum`, `status`, `error` (a maioria já prevista em `erd-documentos.md`) |
| `Chunk → Document → Event → Process → Court` | `chunk → document → court_record → court_case` | **REUSE** — proveniência jurídica idêntica |
| Pipeline OCR/chunk/embedding (§17 do discovery) | slice `indexing` + `document` (Claude vision p/ OCR, Voyage `voyage-law-2` p/ embeddings, portas `TextExtractor`/`Embedder`) | **REUSE** — **não** construir pipeline paralelo |
| `ProcessSyncJob` | `sync_run` (audit de sync: `court_record_id`, `connector_id/version`, `status`, `items_new/deduped`, `window_*`, `error`) | **REUSE** — os estados finos do discovery (`AUTHENTICATING`, `FETCHING_EVENTS`…) são progresso do connector/task asynq, não colunas novas |
| `CourtConnection` (auth/sessão/cert/MFA por advogado) | `integration` (assinatura de fonte por tenant, `source`, `scope.oab`, `credential_ref`) é a base; **não** modela sessão/MFA/certificado | **CREATE `court_connection`** — especializa `integration` para portal autenticado (estado de sessão é genuinamente novo) |
| `FilingJob` + `CourtPetition` | `petition` (imutável, `receipt`, `observed_result`) + `draft.saga_state` (`…→SIGNED→FILED→LABELED`) | **REUSE** — filing é a transição `SIGNED→FILED` do saga da minuta, executada pelo adapter |
| `NormalizedAction` / `CourtTaxonomyResolver` | `petition.piece_type` é o tipo normalizado; o mapeamento para o código do tribunal **não existe** | **CREATE** — `CourtTaxonomyResolver` no slice `court` |
| Credenciais/segredos | `credential_ref` (ponteiro p/ cofre), convenção "segredo nunca em claro" | **REUSE** — padrão de cofre já é lei no schema |
| Eventos/idempotência | `outbox`/`processed_event`, `trace_context`, `aggregate_id` (uuid) | **REUSE** — todo cruzamento de slice é evento |

**Conclusão:** isto é **EXTEND** do domínio, não greenfield. As únicas tabelas realmente novas são
`court_connection` (estado de sessão autenticada) e, condicionalmente, `filing_attempt` (idempotência de
protocolo — ver §7). Todo o resto reusa.

---

## 3. Princípios (decididos)

1. **Adapter por sistema, core agnóstico** (discovery Decisão 5). O slice `court` expõe `CourtProvider`;
   `EprocAdapter`/`EsajAdapter` decidem por-operação o mecanismo (HTTP, browser, Web Signer). Nenhum outro
   slice importa Playwright.
2. **Slices só falam por evento.** READ emite `court.autos_fetched` → `document`/`indexing` ingerem. WRITE
   é disparado por `draft.filing_requested` e responde com `court.filing_confirmed`. `court` nunca importa
   entity/repo de `draft`; só o contrato de evento.
3. **LLM nunca toca credencial/certificado** (discovery §26). O contexto do LLM é `chunk`/`docket_entry`;
   o cofre e o worker de execução são outra fronteira. `credential_ref`/`certificate_ref` são ponteiros.
4. **Nunca `LLM → Tribunal`.** Peticionamento é sempre `draft` aprovado por humano/policy antes de
   `SIGNED→FILED` (`erd-pecas.md §3.1`). Human-in-the-loop é **requisito de compliance**, não feature.
5. **`UPLOAD_SUCCESS ≠ PROTOCOL_SUCCESS`** (discovery §24). `HTTP 200` não é protocolo. Só é `CONFIRMED`
   com `protocol_number + timestamp + receipt`, e — quando possível — **segunda confirmação** consultando
   o próprio processo (a peça apareceu nos autos?). Isso já casa com `petition.receipt`/`observed_result`.
6. **Filing idempotente** (novo, crítico). Nunca blind-retry de estado `SUBMITTING` desconhecido — pode
   protocolar em duplicidade nos autos. Reconciliar-por-consulta antes de reenviar (§7).
7. **Sync incremental por cursor** (discovery §18). Reusa `docket_entry.observed_at` + `sync_run.window_*`;
   baixa só evento/documento novo. O conceito de cursor existe mesmo sem MNI.
8. **Autos como eventos+documentos** (discovery §15), não `PDFs[]`. Já é `docket_entry → document`.
9. **Resolver sistema por processo, não por tribunal** (discovery §5). `eproc` vs `e-SAJ` depende de
   foro/competência/estágio de migração e **pode mudar no meio da vida** — reavaliado por sync.

---

## 4. Onde vive na arquitetura (slices)

O discovery fala em "Engines" (horizontal); a plataforma é **vertical-slice**. Tradução:

```
acquisition (existe)   descobre/consolida por DJEN/DATAJUD → court_record/docket_entry/intimation
        │  (intimation.observed / court_record_observed)
        ▼
court (NOVO)           CourtConnection + CourtProvider(eproc|esaj)
        │                 ├── READ:  fetch autos → court.autos_fetched
        │                 └── WRITE: assina+protocola → court.filing_confirmed
        ▼
document + indexing (existe)   ingere autos (extração/OCR/chunk/embedding) → document.ready
        ▼
draft/review/petition (existe) RAG dos chunk → minuta → revisão → aprovação
        │  (draft.filing_requested quando saga chega em SIGNED)
        └──────────────► court (WRITE) ──► petition.receipt + observed_result
```

**Responsabilidades do slice `court`:** `court_connection` (auth/sessão/MFA/cert refs), os adapters, o
`CourtTaxonomyResolver`, e os dois casos de uso (FetchAutos, Filing). **Não** possui `document`, `chunk`,
`draft` nem `petition` — reage/produz eventos.

`cmd/` novo: `worker-court` (execução autenticada isolada — cofre + browser/HTTP), separado do
`worker-documents`/`worker-ai` por segurança (menor privilégio: só ele fala com o tribunal e o cofre).

---

## 5. `court_connection` (tabela nova) & resolução de sistema

`integration` continua sendo a **assinatura de fonte por tenant** (DATAJUD/DJEN por OAB). O acesso
autenticado ao portal é **por advogado × tribunal × sistema**, com estado de sessão/MFA/certificado que
`integration` não modela. Daí uma tabela dedicada:

```sql
CREATE TABLE court_connection (
  id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id              uuid NOT NULL REFERENCES tenant(id),
  app_user_id            uuid NOT NULL REFERENCES app_user(id),   -- o advogado dono da identidade
  court                  text NOT NULL,                            -- TJSP (v1: só TJSP)
  system                 text NOT NULL,                            -- EPROC | ESAJ
  authentication_method  text NOT NULL,                            -- PASSWORD | CERTIFICATE_A1
  credential_ref         text,                                     -- ponteiro cofre (login/senha) — nunca em claro
  certificate_ref        text,                                     -- ponteiro cofre (A1) — nunca em claro
  session_ref            text,                                     -- ponteiro cofre (cookies/token de sessão/device-trust)
  status                 text NOT NULL DEFAULT 'DISCONNECTED',
  last_authenticated_at  timestamptz,
  last_sync_at           timestamptz,
  error                  jsonb,
  created_at             timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, app_user_id, court, system)
);
CREATE INDEX ON court_connection (tenant_id);
```

Estados (`status`), a máquina que o discovery §10/§27 pediu:

```
DISCONNECTED → AUTHENTICATING → CONNECTED
                     │
                     ├── MFA_REQUIRED         (eproc 2FA; device-trust perdido)
                     ├── CERTIFICATE_REQUIRED (e-SAJ/Web Signer)
                     ├── REAUTH_REQUIRED      (sessão/política de confiança expirou)
                     └── ERROR
```

**Cofre:** `credential_ref`/`certificate_ref`/`session_ref` seguem o padrão `credential_ref` do schema —
segredo cifrado no cofre, isolado por tenant, jamais em claro na linha, jamais em log/prompt.

**Resolução de sistema (`CourtSystemResolver`).** Não codificar `TJSP → eproc`. A pista vem do que já
consolidamos: `court_record.court`/`judging_body`/`class` + histórico. Onde ambíguo, o próprio fetch
descobre (tenta o sistema provável, cai no outro). A decisão é **reavaliada por sync** — processo migra de
SAJ→eproc no meio da vida. Migração preserva identidade histórica **sem tabela nova**: é o par
`court_record` (mesmo `court_case`) com `lifecycle=SUPERSEDED` no antigo, exatamente o mecanismo
*placeholder+merge* que a consolidação DJEN/DATAJUD já usa (0012).

---

## 6. READ — fetch dos autos (reusa `document`/`indexing`)

Gatilho: `intimation.observed`/`court_record_observed` (já existem) ou pedido manual. O caso de uso
`FetchAutos` no `worker-court`:

```
court_record + court_connection
   → resolve sistema (eproc|esaj)
   → autentica/valida sessão (reusa session_ref; MFA se REAUTH_REQUIRED)
   → lista eventos novos (cursor = max(docket_entry.observed_at))   [incremental]
   → para cada documento novo: download → checksum(sha256) → antivírus
   → grava document (origin=COURT, court_record_id, docket_entry_id) + storage_key
   → emite court.autos_fetched (por documento)  →  document/indexing ingerem
   → grava sync_run (audit: items_new/deduped, window, error)
```

**Deltas em `document`** (a maioria já prevista em `erd-documentos.md §schema`):

```
+ docket_entry_id  uuid REFERENCES docket_entry(id)  -- o "event_id" do discovery (proveniência)
+ external_id      text        -- id do documento no tribunal (dedup incremental)
+ mime_type        text
+ size_bytes       bigint
+ checksum         text        -- sha256 (integridade + dedup)
+ status           text        -- PENDING|DOWNLOADED|EXTRACTING|EXTRACTED|CHUNKED|READY|FAILED
+ error            jsonb
```

**Classe de acesso.** O discovery pediu `PUBLIC/AUTHENTICATED/CONFIDENTIAL/RESTRICTED`. Reusar o enum já
existente `court_record.secrecy` (`PUBLIC|RESTRICTED|SECRET`) como a verdade do processo; "AUTHENTICATED"
não é atributo do doc e sim consequência de ter uma `court_connection`. Não criar enum paralelo.

**Extração/OCR/chunk/embedding:** **não** reimplementar (discovery §17). `court.autos_fetched` entra no
pipeline `document → indexing` que já existe (`has_text_layer` decide OCR; Voyage p/ embedding).

---

## 7. WRITE — assinatura + protocolo (reusa `draft`/`petition`)

O discovery propôs `FilingJob` com FSM própria. Isso **é** o saga da minuta já modelado. Não duplicar:

```
draft.saga_state:  CREATED → EXTRACTING → REVIEWED → SIGNED → FILED → LABELED
                                             │ (aprovação humana/policy — erd-pecas)
                                             ▼
                            draft.filing_requested  ──►  worker-court (Filing use case)
```

`Filing` no `worker-court`:

```
1. CourtTaxonomyResolver: petition.piece_type (normalizado) → tipo/código específico do tribunal
2. autentica/valida court_connection
3. assina (A1 via Web Signer / mecanismo do adapter)
4. submete a petição + anexos
5. captura protocol_number + timestamp + receipt   ← só isto conta como sucesso (Princípio 5)
6. verificação: poll do processo → a peça apareceu nos autos?
7. emite court.filing_confirmed(protocol_number, receipt_ref, hashes)
        → petition grava receipt (imutável) + fecha saga em FILED
```

**Idempotência de protocolo (Princípio 6) — a decisão que evita duplo-protocolo.** Antes de qualquer
reenvio, reconciliar por consulta. Uma tabela leve de tentativa dá o ponto de idempotência que o saga
sozinho não garante (o saga é da minuta; a submissão externa é at-least-once):

```sql
CREATE TABLE filing_attempt (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id          uuid NOT NULL REFERENCES tenant(id),
  draft_id           uuid NOT NULL REFERENCES draft(id),
  court_connection_id uuid NOT NULL REFERENCES court_connection(id),
  idempotency_key    text NOT NULL,          -- derivado de (draft_id, content_hash) — 1 protocolo por peça
  status             text NOT NULL,          -- PREPARING|SIGNING|SUBMITTING|SUBMITTED|VERIFYING|CONFIRMED|FAILED
  protocol_number    text,
  receipt_ref        text,                   -- ponteiro storage do comprovante
  petition_sha256    text,
  receipt_sha256     text,
  error              jsonb,
  started_at         timestamptz NOT NULL DEFAULT now(),
  finished_at        timestamptz,
  UNIQUE (draft_id, idempotency_key)
);
```

Regra: um `SUBMITTING` sem confirmação → **consultar o processo** (a peça já entrou?) antes de reenviar.
`CONFIRMED` só com `protocol_number + receipt`. O `receipt` final vira `petition.receipt` (imutável) e
`observed_result` nasce vazio, preenchido quando o desfecho volta (já é assim no schema).

**Trilha de auditoria** (discovery §25): `filing_attempt` + `petition` já guardam quem/quando/qual peça/
qual protocolo/hashes. Nada novo além dos `*_sha256`.

---

## 8. Eventos (contratos outbox)

Todos na mesma tx do fato, com `trace_context` W3C e `aggregate_id` uuid (convenção do schema):

| Evento | Produtor | Consumidor | Payload essencial |
|---|---|---|---|
| `court.connection_state_changed` | court | UI/notifications | connection_id, status (ex. MFA_REQUIRED → pede 2FA ao advogado) |
| `court.autos_fetched` | court | document/indexing | tenant, court_record_id, docket_entry_id, document_id, storage_key |
| `court.filing_confirmed` | court | petition/notifications | tenant, draft_id, protocol_number, receipt_ref, hashes |
| `court.filing_failed` | court | draft/notifications | tenant, draft_id, kind, error |
| `draft.filing_requested` | draft (existe/estende) | court | tenant, draft_id, court_record_id, petition payload refs |

Idempotência via `processed_event` (at-least-once) — igual ao resto.

---

## 9. Segurança & isolamento

Reforça o padrão do schema, com os requisitos extras do discovery §26:

- **Cofre por tenant**, cifrado em repouso e trânsito, menor privilégio: só `worker-court` lê segredo.
- **Nunca** senha/PIN em log; **nunca** certificado no contexto do LLM; **nunca** credencial em prompt.
- Rotação/revogação e **exclusão segura** de `credential_ref`/`certificate_ref` (LGPD + custódia A1).
- RLS + `SET LOCAL app.tenant_id` por tx (2 barreiras), como toda tabela de usuário.
- `tenant_id` em `court_connection`/`filing_attempt`, vindo do token, nunca do body.

---

## 10. Estratégia de implementação (entregáveis testáveis E2E)

O discovery empacotou tudo em "P0 eproc" (o produto inteiro). Fatiado em entregáveis **verticais**, cada um
com valor observável e um teste de aceite E2E. **De-riscar leitura antes de escrita.**

### 10.1 O que torna cada fatia E2E-testável: dois portões

- **Portão A — E2E-produto (determinístico, CI).** Um `fakeCourtProvider` implementa a interface
  `CourtProvider` e devolve autos/protocolo canônicos. Prova **toda a fiação** (credencial → evento →
  ingestão → grounding → minuta → filing → recibo) sem tocar no tribunal. Análogo do `fakeOutbox`. **Primeiro
  artefato do slice `court`** — habilita testar as fatias de baixo sem portal real.
- **Portão B — E2E-realidade (manual, homologação/portal real).** Prova que o **adapter real** funciona
  contra o eproc. Não roda em CI (rate-limit + credencial + cert). Validação por walkthrough/homologação.
  O adapter real é trocado atrás da mesma interface; a fiação provada no Portão A não re-quebra.

### 10.2 Entregáveis

**Fase 0 — esqueleto andante (mata UNKNOWNs antes de investir)**

| # | Entrega | Aceite E2E |
|---|---|---|
| **D0** | `lib/eproc` walking skeleton: login SSO (`sso.tjsp.jus.br`) + sessão + listar 1 processo + eventos + baixar 1 doc. Sem DB/cofre (cred de teste por env). Reusa transporte uTLS+proxy do DJEN. | **B** — roda contra processo real JEC Franca/Ribeirão; despeja o que o portal expõe. Resolve os 2 UNKNOWNs: CPF/CNPJ sem procuração + incidência de captcha/MFA nos autos. |

**Fase P0a — eproc READ (autos entram) — MARCO SHIPPÁVEL, caminho comprometido**

| # | Entrega | Aceite E2E |
|---|---|---|
| **D1** | `court_connection` + `lib/vault` + endpoint connect/validate (valida cred síncrono contra SSO real). Re-aterrissa a fatia revertida. | **B** — tela Integrações → salvar → status `CONNECTED`. Walkthrough headed + SSO real. |
| **D2** | `worker-court` + FetchAutos + `EprocAdapter` READ + `court.autos_fetched` → pipeline `document`/`indexing`. Nasce o `fakeCourtProvider`, o anti-bot (uTLS+proxy), o `CourtSystemResolver` (trivial: só eproc). | **A** — fake adapter → evento → `document`/`chunk` no pgvector (testcontainers, CI). **B** — adapter real busca autos de processo real → indexados. |
| **D3** | FE — autos no Cockpit (aba Documentos mostra docs `COURT` + proveniência `docket_entry`; status/erros de sync). | **B** — walkthrough: dispara sync, autos aparecem, abre um doc. |

> **Fim da P0a = valor cobrável:** grounding do LLM enriquecido com autos reais, sem tocar em assinatura.

**Fase P0b — eproc WRITE — desenhado, GATED (parecer legal §11 item 1 + descobrir homologação §10.3)**

| # | Entrega | Aceite E2E |
|---|---|---|
| **D4** | `CourtTaxonomyResolver`: `petition.piece_type` → tipo/código eproc. Domínio puro + tabela de mapeamento. | **A** — golden test contra tipos reais de petição do eproc (CI). |
| **D5** | `filing_attempt` + Filing use case + `EprocAdapter` WRITE (assina A1 → submete → `protocol_number`+`receipt` → poll de verificação) → `court.filing_confirmed` → `petition.receipt`. Disparado por `draft.filing_requested`. | **A** — fake adapter: saga `SIGNED`→filing→recibo; **idempotência provada** (2 disparos → 1 protocolo). **B** — filing real contra homologação eproc + cert de teste. |
| **D6** | FE — aprovação human-in-the-loop → dispara filing → mostra protocolo + recibo + verificação. | **B** — walkthrough em staging com fake adapter. |

**Fase P1 — e-SAJ — DEFERIDA (após gate Web Signer §11.2)**

| # | Entrega | Aceite E2E |
|---|---|---|
| **D7** | `EsajAdapter` READ (reusa FetchAutos). | **A** fake + **B** real. |
| **D8** | `EsajAdapter` WRITE + Web Signer. **Bloqueado** pelo spike de concorrência; pode virar Desktop Agent (fora do MVP). | — |

### 10.3 Decisões travadas (sessão 2026-08-17)

- **Sequência:** shippar **P0a primeiro** (D0→D3); P0b só depois de P0a em produção. P1/P2 deferidos.
- **Homologação eproc + cert de teste:** *desconhecido* — **TODO de descoberta** que bloqueia só o
  **Portão B do D5** (write real). Não bloqueia P0a nem os testes Portão A do D5.
- **P2 — outros tribunais / MNI adapter opcional:** MNI nunca foi dependência (`pje-mni` pausado). O
  `CourtProvider` deixa migrar operação portal → MNI sem tocar o core.

---

## 11. Gates que podem mudar o escopo (elevar de "spike" para pré-condição)

O discovery listou estes como "itens do spike". Três são **bloqueadores potenciais** — decidir **antes**
de comprometer escopo, porque são a mesma classe de fricção institucional que já pausou o MNI:

1. **Legal / ToS / ética OAB.** Guardar A1+PIN e agir como o advogado (login robótico + protocolo) pode
   esbarrar em termos de uso dos portais TJSP e em regras ICP-Brasil de custódia de certificado.
   "Tecnicamente viável" ≠ "permitido". **Gate:** parecer jurídico + consentimento explícito do advogado
   antes do P0b (assinatura). Human-in-the-loop é exigência de compliance.
2. **Web Signer em cloud (e-SAJ).** Exige native host local + extensão + cert no store do SO; rodar
   server-side com concorrência de N advogados é genuinamente difícil. **Gate:** provar num spike isolado
   **antes** de comprometer o P1 e-SAJ. Se não escalar → Desktop Agent (fora do MVP) ou fallback 3º-party.
3. **MFA / 2FA (eproc) — RESOLVIDO (mecanismo confirmado com o cliente, 2026-08-18).** O eproc exige TOTP
   tanto pra **acessar o processo** quanto pra **peticionar**. Solução (a que o Legal Mail já usa em
   produção): no enroll do 2FA o tribunal mostra um **QR code** que codifica o **seed TOTP** (`otpauth://…
   ?secret=…`); em vez de escanear no celular, captura-se o **print do QR uma vez**, o sistema **decodifica
   o seed e o armazena cifrado**, e passa a **gerar os códigos de 6 dígitos programaticamente** (RFC 6238) —
   sem celular, sem humano, sem device-trust frágil. Não é mais gate de escopo; é **padrão conhecido**.
   Implicação: `court_connection` ganha um slot de **seed TOTP por (advogado, tribunal)** no cofre; o
   onboarding decodifica o QR → seed. Custódia sobe de nível (guardamos A1 **+** o segundo fator) → reforça
   o gate 1 (consentimento/legal) e o cofre. Caveat: armazenar o seed "anula" o intuito do 2FA (segundo
   dispositivo) — é prática comercial estabelecida (Legal Mail), aceita pelo cliente, mas registrar no
   parecer legal do gate 1.

E os riscos técnicos do discovery §33 que continuam valendo — com um que o discovery **omitiu**:

4. **Anti-bot / IP / fingerprint TLS.** O projeto **já apanhou disso** no DJEN: WAF 403 em IP de
   datacenter Railway, throttle por fingerprint JA3, necessidade de proxy residencial BR. Scraping
   **autenticado** é mais sensível ainda. **Reusar o aprendizado:** proxy residencial/BR por
   `court_connection`, uTLS/Chrome fingerprint, session pinning por advogado. Não é opcional em produção.

---

## 12. Fallback 3º-party — decisão a revisitar pós-spike

O discovery cravou "infra própria, sem depender de Legal Mail/Escavador" (Decisão 1). Dado o risco Web
Signer/MFA, um **híbrido** pode ser o MVP pragmático: infra própria para READ; fallback 3º-party
(Escavador/Legal Mail) só no caminho duro de **assinatura/protocolo**. O `CourtProvider` já permite um
`ThirdPartyAdapter` como implementação alternativa por-operação. **Rebaixar** de "não é dependência" para
"decisão a revisitar após o spike de assinatura", atrás de um flag por `court_connection`.

---

## 13. Critério de encerramento do spike

Solução tecnicamente validada quando, em homologação sempre que houver, rodar ponta a ponta:

```
court_connection (auth) → fetch processo → fetch eventos → download docs
  → (pipeline document/indexing) → chunk/embedding
  → draft (RAG) → review → aprovação humana
  → assinatura → protocolo → protocol_number + receipt → verificação (peça nos autos) → CONFIRMED
```

em `TJSP/eproc` (P0) e `TJSP/e-SAJ` (P1, após gate Web Signer). Sucesso = `filing_attempt.CONFIRMED` com
`protocol_number + receipt`, e a segunda confirmação consultando o próprio processo.

---

## 14. Resumo das mudanças de schema

| Objeto | Tipo | Nota |
|---|---|---|
| `court_connection` | **tabela nova** | estado de sessão autenticada por advogado×tribunal×sistema |
| `filing_attempt` | **tabela nova** | idempotência + audit do protocolo externo (at-least-once) |
| `document` | **deltas** | `docket_entry_id`, `external_id`, `mime_type`, `size_bytes`, `checksum`, `status`, `error` |
| `integration.source` | **enum estende** | `+ ESAJ`, `+ EPROC` (já previa "v1+ MNI, ESAT...") |
| `court_case`/`court_record`/`docket_entry`/`chunk`/`sync_run` | **REUSE** | migração SAJ→eproc = placeholder+merge (0012), sem tabela nova |
| `draft`/`review`/`petition` | **REUSE** | filing = transição `SIGNED→FILED` do saga; `petition.receipt`/`observed_result` já existem |

---

## 15. Referências

- `docs/erd-modelo-de-dados.md` — schema (fonte de verdade).
- `docs/erd-documentos.md` — pipeline extração/OCR/chunk/embedding (a fatia COURT nasce aqui).
- `docs/erd-pecas.md` — ciclo `draft/review/petition`, human-in-the-loop, `receipt`/`observed_result`.
- `docs/erd-ai-advisory.md` — miolo de IA (grounding, agentes) que consome os `chunk`.
- `tjsp-technical-discovery.md` — discovery documental de origem (17/08/2026).
- CNJ MNI 3.0; TJSP e-SAJ/eproc; Softplan Web Signer — validação prática pendente (spike, §11).
