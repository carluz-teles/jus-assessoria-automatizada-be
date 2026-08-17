# ERD — Scraping do Portal do Tribunal (automação de navegador, read-only)

> **Status:** desenho (v0 → habilita **partes reais (CPF/CNPJ)**, **autos oficiais** e **andamentos** no
> Cockpit do Processo e no pipeline de IA, consultando o **portal do tribunal (eproc TJSP)** por
> **automação de navegador** com a **credencial pessoal de login do próprio advogado** — a mesma que ele já
> usa no dia a dia). É o **primeiro produtor real** de `party.document` e de `document.origin=COURT`.
> **100% leitura nesta fatia** — nenhuma escrita/peticionamento no portal.
> **Substitui** `docs/erd-pje-mni.md` (PAUSADO) como plano de v0: o credenciamento institucional MNI é uma
> barreira burocrática por-escritório (ofício à Presidência, semanas), limite estrutural do setor. Decisão
> do dono do produto: **a ingestão é commodity/meio**; o diferencial é a camada de IA sobre o dado. O
> concorrente "Legal Mail" já faz assim hoje com o cliente-alvo — robôs logando com a conta do usuário nos
> portais. *"Da forma que puxamos os autos, tanto faz."*
> **Fonte de verdade do schema:** `erd-modelo-de-dados.md` (`integration`, `party`, `party_counsel`,
> `document`, `court_record`, `court_case`, `docket_entry`, `app_user`). Onde divergir, o schema vence.
> Complementa `erd-documentos.md` (a porta `DocumentSource` que aqui recebe seu 1º adapter), reaproveita o
> reuse-check de `erd-pje-mni.md` (o domínio `internal/acquisition` acomoda scraping como mais um `source`,
> exatamente como acomodaria o MNI) e não implementa peticionamento (`FilingGateway`/`Signer` de
> `erd-pecas.md`, fatia futura).

---

## 1. Contexto & objetivo

Hoje o dado de processo nasce do **DJEN** (descobre por OAB) e do **DATAJUD** (enriquece por número).
Nenhum entrega o que o advogado mais precisa: **DJEN nunca revela CPF/CNPJ** (LGPD — `party.document` é
sempre NULL, `connector.go:104`), **DATAJUD não entrega documentos dos autos** (`erd-documentos.md §5`), e
ambos são *observação passiva do diário*, não a consulta aos autos. `document.origin=COURT` está **nomeado
mas nunca populado** (`internal/document/entity.go:61`); Documentos v0 vive só de UPLOAD.

O canal oficial (MNI/SOAP) resolveria isso, mas exige credenciamento institucional por-escritório
(burocrático, semanas). **Pivô do dono do produto:** obter o mesmo dado por **automação de navegador
contra o portal** (eproc TJSP), autenticando com a **credencial pessoal do advogado** — que ele já possui,
sem burocracia de onboarding.

**Objetivo:** um conector **`source=SCRAPER`** dentro de `internal/acquisition` (mesmo `Connector`/`Parser`/
`Orchestrator` de DJEN/DATAJUD) que, para `court_record` **elegível** (TJSP eproc-nativo) **e** com uma
**credencial de portal configurada pelo advogado responsável**, faz login no portal, consulta os autos, e
traz: **partes com documento (CPF/CNPJ quando o portal expõe)**, **andamentos** e **metadados/inteiro teor
dos documentos** (alimentando `document.origin=COURT` via a porta `DocumentSource`). Credencial
**configurada explicitamente pelo advogado** no Painel de Integrações; consulta **automática por-processo
elegível** depois disso (mesma filosofia dos re-polls de `scheduler.go`).

**O que este slice NÃO faz (v0):** escrever/peticionar/assinar no portal, resolver captcha
automaticamente, ou consultar qualquer tribunal além de **TJSP eproc**. Peticionamento é
`FilingGateway`/`Signer` (`erd-pecas.md`) e permanece sujeito a *"IA nunca protocola sozinha, sempre
aprovação humana"* — inegociável mesmo fora do v0.

---

## 2. Reuse-check (Regra nº1)

Reaproveita integralmente o reuse-check de `erd-pje-mni.md §2` (o domínio de aquisição acomoda um canal
novo como `source`, sem slice novo). Verificado por grep nesta sessão:

| Procurei por | Achei (evidência) | Decisão |
|---|---|---|
| conector de fonte externa | `connector.go`: `Connector`(`ID`,`Version`,`Capabilities`,`Fetch(ctx,FetchRequest)→RawPayload`), `Parser`(`CanParse`,`Parse→ParsedResult`), `ParserSet`; `orchestrator.Register(source,c)` (`cmd/worker-ingestao/main.go:152-193`) | **EXTEND** — scraper é mais um `source`; **não** criar slice nem porta de conector novos |
| enum `source` + `integration` | `entity.go:16-20` (`SourceDJEN/DATAJUD/Upload`, **sem** scraper); `migrations/0001` `integration(tenant_id, source)` **UNIQUE**, `credential_ref text` ("null in v0"), `status ...AUTH_FAILED...`, comentário reserva `ESAJ` | **EXTEND** — `SourceScraper` entra como constante nova; ⚠️ mas o `UNIQUE(tenant_id, source)` **não comporta N credenciais por advogado** (§4) |
| `FetchRequest` sem credencial | `connector.go:42` — *"carries no credentials — the connector resolves those from its own configuration (credential_ref)"* | **REUSE** — o conector resolve a credencial atrás de `credential_ref`, igual ao previsto |
| resultado agnóstico de fonte | `ParsedResult{CourtRecords,DocketEntries,Intimations,Parties}`; `ParsedParty{Role,Name,Counsels}` (`connector.go:106`) sem `Document` | **EXTEND** — só falta **preencher `document` da parte** e a **lista de documentos dos autos** (mesmo delta do MNI, §7) |
| partes reais | `party.document` (0032) sempre NULL (`entity.go:181`); `party.source` aceita `DJEN`/`MANUAL` (`entity.go:173-176`) | **EXTEND** — scraper é o **1º produtor** de `party.document`; `source` ganha `SCRAPER` |
| documento dos autos | `document.origin=COURT` (`internal/document/entity.go:61`) nomeado, nunca populado; porta `DocumentSource` desenhada em `erd-documentos.md §5`, sem adapter | **EXTEND** — scraper é o **1º adapter de `DocumentSource`** e o 1º produtor de `origin=COURT` |
| **vínculo processo↔advogado responsável** | ✅ **existe**: `court_case.assigned_user_id uuid REFERENCES app_user(id) ON DELETE SET NULL` (**migration 0029**), write path `PUT /v1/processos/:id/responsavel` (`queries/responsible.sql`). **Nullable** (pode não ter responsável) | **REUSE** — é a chave para escolher a credencial por-processo (§4, AMBIGUITY #1 resolvida) |
| gatilho por evento / re-poll | `listener.go`, `scheduler.go` (`RunDuePoll` por `court_record.next_sync_at`, `WithResyncInterval`) | **REUSE** — o sync do scraper é disparado pelos MESMOS eventos; nenhum trigger novo |
| cofre de segredo por-tenant | ⚠️ **zero groundwork**: grep por `pgcrypto/aes/encrypt/envelope/KEK/DEK/vault/kms` em `lib/`,`internal/`,`migrations/` → **nada**. `lib/config` só env var process-wide | **CREATE** — `credential_ref` ganha seu 1º produtor; precisa de cofre (§6) |
| navegador headless / RPA / parser HTML | ⚠️ **zero groundwork**: grep por `chromedp/playwright/rod/goquery/colly/selenium/headless` → **nada** | **CREATE** — `lib/scraper` (motor + gerenciamento de sessão) é o maior trabalho novo (§5) |
| evasão de anti-bot (já no repo) | ✅ **existe parcial**: DJEN connector usa **uTLS Chrome fingerprint + HTTP CONNECT proxy residencial** (`djen.go:16,86-95,183-192`, `WithDJENProxy`) contra WAF por JA3/IP | **REUSE** — o mesmo padrão (uTLS + proxy BR) se aplica ao scraper HTTP; ver §5 |
| `app_user` como advogado | `migrations/0001:23-31` — `app_user(id, clerk_user_id, tenant_id, email, role ADMIN\|LAWYER)`. ⚠️ **não** guarda OAB nem identidade de portal (a OAB vive em `integration.scope`) | **REUSE + delta** — a credencial de portal é uma tabela nova ligada a `app_user` (§4) |

**Conclusão:** o domínio de aquisição está pronto para receber o scraper (conector/parser/orchestrator/
eventos/re-poll não mudam de forma), e o vínculo processo↔advogado (`court_case.assigned_user_id`) já
existe para a escolha de credencial. O trabalho **novo** é: (a) `lib/scraper` — motor de automação + sessão
autenticada (zero groundwork; login precede toda consulta — responsabilidade que MNI **não** tinha); (b) o
**cofre de credencial POR-ADVOGADO** (`portal_credential`, não `integration`) — a senha é **pessoal**,
classe de risco maior que a institucional do MNI; (c) o **mapa de elegibilidade eproc** (reaproveitado do
MNI); (d) preencher `party.document` e `origin=COURT` (deltas mínimos em `ParsedParty` + adapter
`DocumentSource`); (e) a **taxonomia de erro** visível ao produto (§8).

---

## 3. Princípios (decididos)

1. **Scraper é um `source`, não um domínio novo.** Conector/parser/orchestrator/eventos/re-poll de
   `acquisition` já são a forma certa (EXTEND, nunca INTRODUCE — a skill manda preferir isso). Um pipeline
   paralelo duplicaria sync/idempotência/outbox.
2. **100% leitura no v0.** Login → consulta → parse. **Zero escrita** no portal (mais inegociável que no
   MNI: o formulário de protocolo real muda mais, um erro de preenchimento gera ato processual indevido, e
   não há schema estruturado — como um SOAP teria — para validar).
3. **Credencial POR-ADVOGADO, explícita, consulta automática.** A senha é **pessoal** de cada advogado; o
   advogado a configura (ação humana, uma vez) no Painel de Integrações. Sem configuração → **nenhuma
   tentativa de scraping** para os processos daquele advogado (ACCEPTANCE #1). Consulta por processo
   elegível é automática depois.
4. **A credencial de um processo é a do seu responsável.** Qual credencial usar por-processo sai de
   `court_case.assigned_user_id` (§4). Sem responsável atribuído, ou responsável sem credencial → o
   processo **não é scrapeado** (viés conservador; nunca chuta credencial).
5. **Elegibilidade antes de rede.** Nenhuma tentativa contra processo em eSAJ residual. eproc-nativo × eSAJ
   é resolvido **localmente** (mapa `eproc_coverage` + `filed_at`) antes do `Fetch` (ACCEPTANCE #3).
6. **Credencial nunca em claro.** `credential_ref` aponta para o cofre cifrado; a senha pessoal nunca
   aparece em código, log, outbox, trace ou `scope`. Vazamento permite **qualquer ação que o advogado
   faria logado** — classe de risco mais alta que o MNI (§10).
7. **Captcha não se resolve — sinaliza-se.** Nunca tentar quebrar captcha automaticamente (evita corrida
   armamentista e risco de bloqueio da conta/IP). Consulta bloqueada por captcha → estado categorizado; se
   recorrente, sinaliza degradação (edge case do PM #2).
8. **Degradação honesta, dado sempre presente.** Falha de qualquer categoria **nunca** deixa o Cockpit sem
   dado — sempre o **último dado bom** com `last_synced_at` visível (ACCEPTANCE #3). A UI mostra "última
   sincronização" e fonte discreta por seção — **nunca** um selo comparativo de confiabilidade (edge #5).
9. **Cadência conservadora.** Scraping contra portal de tribunal pode bloquear a conta do advogado por uso
   automatizado. Não é "quanto mais rápido melhor" — `next_sync_at` do scraper é **mais espaçado** que o
   DJEN, jitterado, e serializado por advogado (§10).
10. **Cobertura cresce com o tempo.** O rollout eproc é progressivo por região (~4 anos); `eproc_coverage`
    é **dado versionado** (tabela seed), não código.

---

## 4. Modelo de dados (a decisão estrutural desta fatia)

### 4.1 O problema: `integration` é `UNIQUE(tenant_id, source)` — não comporta N credenciais

O MNI (`erd-pje-mni.md`) assumia **uma** credencial institucional por-tenant, que cabe na linha
`integration(tenant, 'MNI')`. **Aqui é diferente:** a senha é **pessoal, por-advogado** — um escritório com
5 advogados tem 5 credenciais de portal distintas. O `UNIQUE(tenant_id, source)` de `integration`
(`migrations/0001:76`) **admite apenas uma linha** `(tenant, 'SCRAPER')`. Forçar N credenciais dentro dela
(ex.: um array de segredos no `scope`) quebraria o modelo 1-linha-por-fonte e misturaria N segredos numa
`credential_ref` única. **É um problema real de modelagem que esta fatia resolve.**

### 4.2 DECISION — tabela nova `portal_credential` (1:N por advogado), + `integration` fica como o "master switch"

**OPÇÃO A — Estender `integration` para 1:N** (dropar o UNIQUE, permitir N linhas `(tenant, 'SCRAPER')`,
uma por advogado).
- **Contra:** `integration` é semanticamente *"a subscrição do tenant a uma fonte"* (uma por fonte);
  virá-la N-por-fonte contamina todo o código que assume 1 linha (`orchestrator`, `scope`, o read model de
  Integrações). Quebra invariante existente por conveniência.

**OPÇÃO B — Tabela nova `portal_credential`, ligada a `app_user`, com `integration` como master switch
(recomendado).**
- A linha `integration(tenant, 'SCRAPER')` continua sendo **uma** — é o *"o escritório ativou scraping"*
  (liga/desliga, `status`, `scope` mínimo). As **N credenciais pessoais** vivem em `portal_credential`,
  cada uma ligada a um `app_user` (o advogado dono da senha).
- **Pró:** preserva a semântica de `integration`; modela corretamente o 1:N; `credential_ref` do
  `portal_credential` aponta para o cofre (§6); RLS e `assigned_user_id` casam naturalmente.

**DECISION = Opção B.** Preserva o invariante de `integration`, modela o 1:N real, e o join
`court_case.assigned_user_id → portal_credential.app_user_id` resolve a escolha de credencial por-processo.

```sql
-- portal_credential — a credencial PESSOAL de portal de UM advogado (app_user), por tribunal.
-- N por tenant (uma por advogado × portal). O segredo (login/senha) NÃO fica aqui — fica cifrado
-- em tenant_secret (§6); credential_ref aponta a linha do cofre. Aqui só metadados NÃO-secretos.
CREATE TABLE portal_credential (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id      uuid NOT NULL REFERENCES tenant(id),
  app_user_id    uuid NOT NULL REFERENCES app_user(id) ON DELETE CASCADE,  -- o advogado dono da senha
  portal         text NOT NULL,          -- 'TJSP_EPROC' (aberto p/ ESAJ/PJE/PROJUDI no futuro)
  login          text NOT NULL,          -- o usuário de login (CPF/e-mail) — NÃO é secreto; a SENHA sim
  credential_ref text NOT NULL,          -- pointer p/ tenant_secret (a senha cifrada); 1º produtor real
  status         text NOT NULL DEFAULT 'ACTIVE',   -- ACTIVE|AUTH_FAILED|CAPTCHA_BLOCKED|DISABLED
  last_error     text,                   -- categoria do último erro (taxonomia §8) — visível à UI
  last_verified_at timestamptz,          -- último login bem-sucedido (UI: "conectado como <login>")
  configured_by  uuid REFERENCES app_user(id),  -- quem configurou (auditoria/consentimento)
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, app_user_id, portal)   -- uma credencial por advogado × portal
);
CREATE INDEX ON portal_credential (tenant_id);
CREATE INDEX ON portal_credential (app_user_id);   -- FK manual (PG não auto-indexa)
-- RLS: tenant_isolation (SET LOCAL app.tenant_id), como toda tabela per-tenant.
```

### 4.3 Escolha de credencial por-processo (AMBIGUITY #1 — resolvida tecnicamente)

O vínculo confiável **existe**: `court_case.assigned_user_id` (migration 0029, `PUT /processos/:id/
responsavel`). A elegibilidade de scraping de um `court_record` resolve a credencial assim:

```
credencial_do_processo(court_record) :=
    court_case(court_record.case_id).assigned_user_id  →  app_user
    →  portal_credential WHERE app_user_id = <esse> AND portal = 'TJSP_EPROC' AND status = 'ACTIVE'
```

- **Sem `assigned_user_id`** (nullable — processo sem responsável) → **não scrapeia** (viés conservador; a
  UI pode sugerir "atribua um responsável com credencial de portal para sincronizar os autos").
- **Responsável sem `portal_credential ACTIVE`** → não scrapeia; sinaliza no Cockpit.
- **RECOMMENDATION (volta ao produto, não bloqueia o v0):** um fallback opcional — "credencial padrão do
  escritório" (uma `portal_credential` marcada como default do tenant) para processos sem responsável — é
  decisão de produto, **não** implementar sem pedido explícito (evita usar a senha de alguém em processo
  que não é dele).

### 4.4 Sem DDL estrutural novo em `party`/`document`/`court_record` (reaproveitado do MNI)

- **`party`/`party_counsel`** (0032) — scraper é o **1º produtor de `party.document`**. Sem DDL: passa a
  **escrever** `document` (CPF/CNPJ) e `source='SCRAPER'`; a UNIQUE `(tenant, case, role, name)` faz o
  upsert idempotente (enriquece a parte que o DJEN já criou pelo nome). Precedência: SCRAPER (documento
  real dos autos) **sobrepõe** a linha do DJEN.
- **`document`/`chunk`** (0001) — scraper é o **1º produtor de `origin=COURT`**. Sem DDL estrutural: o
  adapter `DocumentSource` (§7) cria `document` com `origin=COURT`, `court_record_id` preenchido, e dispara
  o pipeline de extração/chunk existente (`erd-documentos.md`). O byte vai para `lib/storage`, não pela API.
- **`court_record`** (0001) — sem DDL novo. Elegibilidade sai de `court` (=`TJSP`), `cnj_number` (código
  CNJ `.8.26.` = TJSP + comarca), `filed_at` (ajuizamento) + `eproc_coverage` (§4.5).
- **Delta proposto em `court_record`:** `last_synced_source text` + `last_synced_at timestamptz` (nullable)
  — para o Cockpit exibir "última sincronização por fonte" **por seção** (ACCEPTANCE #3, edge #5). *(A
  confirmar no catálogo; alternativa mais barata é derivar do `sync_run` mais recente por
  `(court_record, source)` num read model, sem coluna nova — DEV escolhe.)*

### 4.5 `eproc_coverage` (DDL novo — reaproveitado de `erd-pje-mni.md §4`)

Mapa estático de cobertura eproc por comarca/vara do TJSP (referência global, sem tenant/RLS — é fato
público de rollout), semeado por migration e refinado sob demanda (mesma filosofia do seed de feriados de
`erd-prazos.md`). Schema idêntico ao do ERD MNI — **reaproveitar aquela tabela** (elegibilidade é a mesma
pergunta, independe do canal ser SOAP ou scraping):

```sql
CREATE TABLE eproc_coverage (
  id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  court        text NOT NULL,          -- 'TJSP'
  raj          text,                   -- Região Administrativa Judiciária (ex.: '6') — informativo
  comarca_code text NOT NULL,          -- código CNJ da comarca/foro
  system       text NOT NULL DEFAULT 'EPROC',  -- EPROC | ESAJ
  migrated_at  date NOT NULL,          -- a partir de quando processos nascem nativos nesse system
  note         text,
  UNIQUE (court, comarca_code, system)
);
CREATE INDEX ON eproc_coverage (court, comarca_code);
```

---

## 5. Mecanismo técnico de scraping (DECISION)

Login precede toda consulta — responsabilidade que MNI não tinha. Duas famílias:

**OPÇÃO A — Navegador headless (chromedp/Rod contra Chrome).**
- **Pró:** executa JS, resiste a portais SPA/JS-pesados, "parece" um navegador de verdade; lida com fluxos
  de login multi-passo sem replicar a mão.
- **Contra:** **infra pesada** — precisa de um Chrome no ambiente (Railway workers Go não têm browser
  hoje; imagem enorme, RAM alta, instabilidade operacional). É um salto de infra que a fundação não tem, e
  a skill manda **não introduzir infra por escalabilidade teórica**. Mais lento e caro por consulta.

**OPÇÃO B — Requisições HTTP diretas replicando a sessão do navegador (recomendado v0).**
- **Pró:** **leve** — casa com os workers Go da Railway sem browser; e **o repo já tem o groundwork mais
  difícil**: o DJEN connector usa **uTLS (Chrome fingerprint) + HTTP CONNECT proxy residencial/BR**
  (`djen.go:16,183-192`, `WithDJENProxy`) exatamente para não ser bloqueado por JA3/IP-reputation. O
  scraper reusa esse transporte, adicionando **gerenciamento de sessão** (login → cookies → consulta
  autenticada) e um **parser HTML**.
- **Contra:** mais frágil a proteções anti-bot dinâmicas (JS challenge, tokens gerados em JS); se o eproc
  do TJSP exigir execução de JS no login/consulta, o HTTP puro não alcança e força reavaliar a Opção A
  **só para esse passo**.

**DECISION = Opção B (HTTP direto + uTLS/proxy reusado + sessão + parser HTML), com a Opção A como escape
hatch atrás da mesma porta.** Racional: (1) a fundação não tem browser headless e a skill desaconselha
introduzir a infra sem problema concreto; (2) o repo já provou o padrão uTLS+proxy contra o WAF do DJEN — o
mesmo tribunal-alvo, mesma egress; (3) o eproc TJSP é um portal server-rendered clássico, não um SPA
moderno — HTTP+HTML é plausível. A **primeira fatia (§11) valida exatamente essa premissa** contra o portal
real; se um passo de login exigir JS, a porta `PortalSession` permite trocar **só** aquele adapter por um
headless, sem reescrever o resto.

**WHAT WOULD CHANGE THE DECISION:** o login/consulta do eproc TJSP exigir execução de JS (token dinâmico,
challenge JS) que HTTP puro não replica → aí a Opção A (headless) entra, mas **provada** contra o portal,
não assumida.

**`RawPayload{Body []byte}` basta** — o scraper produz **HTML** (bytes opacos), cabe igual ao JSON do DJEN.
O `Parser.CanParse` casa por `Source == SourceScraper`. **Nenhuma mudança em `RawPayload`/`Connector`/
`FetchRequest`.** O único delta de contrato é `ParsedParty.Document` + `ParsedResult.Documents[]` (§7).

---

## 6. Cofre de credencial pessoal (`credential_ref`) — reavaliado (classe de risco maior)

**O que se custodia:** a **senha pessoal de login do advogado** no portal. Diferente do MNI (par
login/senha **institucional** de consulta), esta senha, vazada, permite **qualquer ação que o advogado
faria logado** — inclusive, em tese, peticionar/assinar dentro da sessão web do portal. É a **classe de
risco mais alta** desta família (mais alta que a credencial de API institucional do MNI, ainda que menor
que custodiar um `.pfx` de identidade digital como arquivo).

**Opções** (reaproveitando `erd-pje-mni.md §6`, reavaliadas à luz da classe de risco maior):

- **OPÇÃO A — Coluna cifrada no Postgres, envelope encryption com KEK em env var (recomendado v0).**
  A senha fica cifrada numa tabela `tenant_secret` (DEK por-segredo, DEK cifrada por KEK de env var —
  `caarlos0/env`, `required`); `portal_credential.credential_ref` aponta a linha.
  - **Pró:** sem infra nova (só Postgres, que já temos — grep confirma zero KMS/Vault); rotação de KEK
    viável; segredo nunca em claro no banco (mesmo em dump); RLS isola por tenant. Mantém o corte deliberado
    do *"Secrets Manager pesado"* (`fundacao-prd:71`).
  - **Contra:** a KEK é env var (mesma superfície do resto dos segredos), não HSM. **A classe de risco
    maior aperta o padrão de aplicação** (§10): read-only rígido, DEK por-segredo, jamais em log/trace, e
    caminho de saída para KMS já previsto (`credential_ref` não muda).
- **OPÇÃO B — Secrets manager dedicado (Vault/AWS Secrets Manager/KMS).**
  - **Pró:** custódia industrial, auditoria, rotação gerenciada — atraente **justamente** porque o material
    é senha pessoal.
  - **Contra:** infra nova que não existe na Railway hoje; custo/operação; contraria a skill (não
    introduzir infra por escalabilidade teórica). **Caminho de saída, não v0.**

**DECISION = Opção A (coluna cifrada, envelope encryption, KEK em env var), com o padrão de manuseio
endurecido pela classe de risco (§10) e Opção B como caminho de migração explícito** (trocar o backend do
cofre sem tocar `credential_ref`). A porta `PortalCredentialProvider` (§7) isola o cofre do resto: hoje
decifra do Postgres; amanhã de um KMS, sem mudar o conector.

> ⚠️ **A classe de risco maior é uma ASSUMPTION a confirmar com o produto:** aceitar custodiar a senha
> pessoal do dia-a-dia num cofre de coluna cifrada (não-HSM) é decisão de negócio. A RECOMMENDATION do PM —
> permitir ao advogado usar **senha dedicada/diferente** da pessoal, se o tribunal suportar múltiplas
> sessões — **reduz o blast radius** e deve ser oferecida na UI de configuração (texto que educa), mesmo
> sendo não-obrigatória no v0.

---

## 7. Portas / interfaces (o que muda no código)

Portas novas + reuso das existentes. Nomes propostos (DEV ajusta):

- **`PortalSession`** (porta **nova** — a responsabilidade que MNI não tinha) — encapsula **login →
  sessão autenticada → consulta**. Login precede toda consulta; a sessão (cookies/token) é **cacheada**
  entre consultas de um mesmo advogado dentro da janela de sync, para não relogar a cada processo (reduz
  carga no portal e blast radius de bloqueio). Sessão expirada no meio → **retry transitório** dentro da
  janela (edge #3), invisível ao usuário a menos que se repita.
  ```
  // Abre (ou reusa do cache) uma sessão autenticada no portal para um advogado, e
  // executa uma consulta read-only. Nunca escreve. Erros são TIPADOS na taxonomia §8.
  Query(ctx, session, cnjNumber) (RawPayload, error)
  ```
  O adapter default é **HTTP+uTLS+proxy** (Opção B, §5); um adapter headless (Opção A) satisfaz a mesma
  porta se um passo exigir JS.
- **`PortalCredentialProvider`** (porta **nova**) — resolve `login`+senha de um advogado a partir de
  `portal_credential.credential_ref`, decifrando do cofre, **sem expor o segredo ao chamador** além do
  necessário para o login. Erro de credencial (senha inválida/expirada) é **tipado** (`apperr`
  `AUTH_FAILED`) e sobe a `portal_credential.status=AUTH_FAILED` + sinal acionável na UI.
  ```
  Credentials(ctx, tenantID, credentialRef) (login, password string, err error)
  ```
- **`DocumentSource`** (porta **já nomeada** em `erd-documentos.md §5`) — **1º adapter aqui**: dado um
  `court_record` + o HTML dos autos, baixa o inteiro teor de cada documento e cria `document` com
  `origin=COURT`. Reusa `lib/storage` + pipeline de extração/chunk.
- **`Connector` scraper** (impl da porta existente `connector.go`) — `ID()="scraper"`, `Version()`,
  `Capabilities()=[FETCH_BY_NUMBER]` (**não** `DISCOVER_BY_OAB` — consulta por número), `Fetch` resolve a
  credencial do advogado responsável via `PortalCredentialProvider`, abre/reusa a `PortalSession`, consulta
  os autos, devolve `RawPayload{Source: SourceScraper, Body: html}`. Registrado no `Orchestrator`
  (`cmd/worker-ingestao/main.go`, ao lado do DJEN/DATAJUD), com functional options no molde do
  `NewDJENConnector` (proxy, rate, base URL).
- **`Parser` scraper** (impl da porta existente) — `CanParse` casa `Source==SourceScraper`; `Parse` faz o
  scrape do HTML → `ParsedResult` (records, docket, **parties com `Document`**, e a lista de documentos dos
  autos para o `DocumentSource`).

**Delta de contrato (o único, idêntico ao MNI):** `ParsedParty.Document string` (CPF/CNPJ, vazio quando o
portal não expõe — ver UNKNOWN §11) + `ParsedResult.Documents []ParsedDocument` (metadados dos autos para o
`DocumentSource`). Round-trip produtor∥consumidor obrigatório ([[parallel-producer-consumer-roundtrip]]).

---

## 8. Taxonomia de erro (obrigatória, visível ao produto)

O PM exige categorias distinguíveis com tratamento de UI distinto. Cada `sync_run` FAILED do scraper e o
`portal_credential.status`/`last_error` carregam **uma** categoria tipada (`apperr` `Kind`):

| Categoria | Sinal técnico | Ação do usuário | UI |
|---|---|---|---|
| **`CREDENTIAL_INVALID`** | login rejeitado pelo portal (senha errada/expirada/conta bloqueada) | **Sim** — reconfigurar a senha | `portal_credential.status=AUTH_FAILED`; "credencial rejeitada, reconfigure" |
| **`CAPTCHA_BLOCKED`** | captcha no login ou na busca | **Não** (não resolvemos captcha) — esperar/reconfigurar | `status=CAPTCHA_BLOCKED`; "consulta bloqueada por verificação; tentaremos de novo". Recorrente → degradação sinalizada |
| **`PORTAL_UNAVAILABLE`** | timeout/5xx/rede/portal fora do ar | **Não** — transitório, re-tenta sozinho | run FAILED, retry na próxima janela; UI só mostra se **repetido** |
| **`PARSING_BROKEN`** | seletores esperados sumiram de forma **consistente e repetida** (estrutura da página mudou) | **Não** — é sinal interno pro time atualizar o scraper | run FAILED categoria distinta; **alerta interno** (observabilidade), não pânico ao usuário |

**Detecção "UI mudou" × "erro transitório" (AMBIGUITY #2 — resolvida com honestidade sobre a imprecisão):**
- **Transitório** (`PORTAL_UNAVAILABLE`): erro de **transporte** — timeout, connection refused, 5xx, TLS.
  Distinguível com alta confiança (é o nível de rede, não de conteúdo). Uma ocorrência isolada → transitório.
- **UI mudou** (`PARSING_BROKEN`): a resposta **chegou 200 OK** mas os **seletores esperados não casam**.
  Distinguível de rede com confiança **quando persiste** — um único parse vazio pode ser um processo
  legitimamente sem aquela seção. **Heurística:** só classificar `PARSING_BROKEN` quando o mesmo seletor
  falha em **N consultas independentes consecutivas** (processos diferentes) — uma falha de estrutura
  atinge *todos*, um dado ausente atinge *um*. Guardar assinaturas de seletor-âncora (ex.: a tabela de
  partes existe? o cabeçalho dos autos existe?) e alarmar quando a taxa de "âncora ausente" cruza um limiar.
- **Honestidade (a skill manda):** **é sempre impreciso**. Um caso incerto (200 OK, parse parcial, sem
  repetição ainda) é classificado como **transitório** (viés conservador — re-tenta em vez de gritar "UI
  mudou"), e só escala para `PARSING_BROKEN` quando o padrão se confirma. **A mensagem ao usuário no caso
  incerto é decisão de produto** (o PM marcou como ambiguidade de produto) — recomendação: "sincronização
  com atraso, tentando novamente", sem culpar o usuário.

---

## 9. Elegibilidade (estoque residual eSAJ × eproc nativo)

Reaproveita a lógica de `erd-pje-mni.md §8`, acrescentando a barreira de credencial por-advogado.
Resolvido **localmente antes de qualquer rede** (ACCEPTANCE #3):

```
elegível_para_scraper(court_record) :=
     court_record.court == 'TJSP'
 AND comarca(court_record.cnj_number) tem linha em eproc_coverage (system='EPROC')
 AND court_record.filed_at >= eproc_coverage.migrated_at da sua comarca
 AND integration(tenant, 'SCRAPER').status == ACTIVE           (escritório ligou o scraping)
 AND court_case.assigned_user_id IS NOT NULL                    (tem responsável)
 AND EXISTS portal_credential(app_user=responsável, portal='TJSP_EPROC', status='ACTIVE')
```

- `filed_at` ausente (DJEN puro, sem DATAJUD) → **não tenta** (viés conservador; opcional: "enriqueça por
  DATAJUD para habilitar"). Nunca chuta elegível.
- A decisão roda no `sync` **antes** do `Fetch` — um predicado `eligibleForScraper` curto-circuita e o run
  nem sai para a rede, mantendo `scheduler.go` sem tentativas falhas repetidas contra eSAJ.

---

## 10. Riscos (segurança, bloqueio, legal)

**Risco central de segurança:** o segredo é a **senha pessoal do advogado**. Vazamento permite qualquer
ação logada (classe mais alta que a credencial de API do MNI). Mitigações:

| Risco | Mitigação proposta |
|---|---|
| Vazamento da senha pessoal | envelope encryption (DEK/KEK, §6), KEK só em env var, nunca em dump; RLS por tenant; **jamais** em log/outbox/trace/`scope`; DEK por-segredo limita blast radius; caminho de saída para KMS (§6 Opção B) sem mudar `credential_ref` |
| Uso indevido além de leitura | v0 é **read-only rígido** — a `PortalSession` só executa consultas; **nenhum** código de escrita/peticionamento existe nesta fatia; peticionamento exige `FilingGateway`+aprovação humana (advisory, inegociável) |
| **Bloqueio da conta do advogado por uso automatizado** (risco real de scraping) | **cadência conservadora**: `next_sync_at` do scraper mais espaçado que o DJEN, com **jitter**; **serializar por advogado** (advisory-lock por `app_user`, no molde do lock por-tenant do DJEN backfill) — nunca N consultas paralelas com a mesma conta; sessão **cacheada** (1 login por janela, não por processo); uTLS+proxy BR reusado (§5) para não destoar de tráfego humano; captcha → **para**, não insiste (princípio 7) |
| Senha rejeitada/expirada | `CREDENTIAL_INVALID` → `status=AUTH_FAILED` + sinal acionável (§8) |
| Consentimento/escopo | o advogado **configura explicitamente** (nunca automático, ACCEPTANCE #1); `configured_by`+timestamp para auditoria; texto de onboarding **antes** do campo de senha explicando proteção e limite de uso (só leitura) (ACCEPTANCE #5) |
| KEK única comprometida | rotação planejada; DEK por-segredo; migração para KMS é caminho de saída |

**Risco legal / Termos de Uso (não-técnico, nomeado, o produto está ciente e aceitando):** automação de
login contra portal de tribunal — **mesmo com consentimento do advogado dono da credencial** — pode
conflitar com os Termos de Uso do tribunal. O concorrente Legal Mail opera assim e seu ToS se isenta
agressivamente de responsabilidade por perda de prazo *"independente da causa"* — sinal de que o mercado
aceita a tensão **e** de que o pipeline é frágil (mudança de UI quebra o scraper). **Não é parecer
jurídico** (fora do meu escopo); é um risco não-técnico que o dono do produto está assumindo
conscientemente, na mesma postura de mercado. Registrado para rastreabilidade.

---

## 11. Pontos de falha & decisões (FACT/ASSUMPTION/UNKNOWN/DECISION)

**DECISION (travadas neste desenho):**
- ✅ Scraper é `source=SCRAPER` **dentro de `internal/acquisition`** (EXTEND do conector/parser), não slice
  novo. `RawPayload`/`FetchRequest`/`Connector` **não mudam**; HTML cabe em `Body []byte`. Único delta de
  contrato: `ParsedParty.Document` + `ParsedResult.Documents[]`.
- ✅ **Credencial POR-ADVOGADO em tabela nova `portal_credential`** (1:N, ligada a `app_user`), **não** em
  `integration` (que fica como master switch do tenant). Resolve o `UNIQUE(tenant_id, source)` (§4).
- ✅ **Escolha de credencial por-processo via `court_case.assigned_user_id`** (existe, migration 0029);
  sem responsável/credencial → não scrapeia (§4.3, AMBIGUITY #1 resolvida).
- ✅ **Mecanismo = HTTP direto + uTLS/proxy reusado + sessão + parser HTML** (Opção B), headless como escape
  hatch atrás da porta `PortalSession` (§5).
- ✅ **Cofre = coluna cifrada (envelope encryption), KEK em env var**, com padrão de manuseio endurecido
  pela classe de risco maior; secrets manager dedicado adiado (§6).
- ✅ **Taxonomia de erro** de 4 categorias tipadas visíveis ao produto (§8); "UI mudou" só após falha
  repetida de seletor-âncora (§8, AMBIGUITY #2 resolvida com honestidade sobre a imprecisão).
- ✅ **Cadência conservadora + serialização por advogado + captcha nunca automático** (§10).
- ✅ v0 **100% leitura**; peticionamento é `FilingGateway`/`Signer` (`erd-pecas.md`), fora do escopo.

**ASSUMPTION (assumido, a validar):**
- Que o eproc TJSP é **server-rendered** e consultável por **HTTP+HTML** (Opção B) — a fatia 1 valida; se
  exigir JS, cai-se no headless atrás da mesma porta.
- Que a UNIQUE `(tenant, case, role, name)` de `party` casa a parte do DJEN com a do scraper pelo nome para
  enriquecer o `document` — variação de grafia é risco a medir (mesma ASSUMPTION do MNI).
- Que aceitar custodiar a **senha pessoal** num cofre de coluna cifrada (não-HSM) é aceitável ao produto
  (§6) — decisão de negócio; oferecer senha dedicada mitiga.
- Que a cadência conservadora + uTLS/proxy evita bloqueio da conta — **sem dado real de incidência**; a
  fatia 3 mede.

**UNKNOWN (crítico, valida antes de implementar o parsing de partes):**
- ⚠️ **O maior (idêntico ao MNI):** *o que o eproc TJSP expõe de **CPF/CNPJ** para um advogado logado **sem
  procuração** nos autos vs. **com** procuração?* O portal pode **mascarar** o documento (ou o processo
  inteiro, por sigilo/segredo de justiça) para quem não é parte/procurador. **Isto muda o ACCEPTANCE #2**
  ("popula party.document"). **Não assumir que vem tudo aberto.** Validar contra o **portal real** com uma
  conta de advogado de JEC de Franca/Ribeirão na **fatia 1**. Se vier mascarado sem procuração, "partes
  reais" fica condicionado a ter procuração/vínculo — decisão de produto a reabrir.
- ⚠️ **Incidência de captcha** (edge #3): captcha recorrente pode **inviabilizar** o tribunal para o v0 —
  é **decisão de negócio a confirmar com dado real** (não é minha para resolver; documentada como
  risco/dependência). A fatia 1 já revela se o login normal cai em captcha.
- ⚠️ **Fragilidade estrutural do scraping:** mudança de UI do portal quebra o parser (o próprio ToS do
  Legal Mail sinaliza isso). Mitigado pela taxonomia (`PARSING_BROKEN` → alerta interno) + fallback ao
  último dado bom, mas é um **custo de manutenção recorrente** que o produto assume.

**FACT (evidência de código/negócio):**
- `party.document` sempre NULL (`connector.go:104`, `entity.go:181`); `document.origin=COURT` nomeado,
  nunca populado (`internal/document/entity.go:61`); `integration.credential_ref` sem produtor
  (`0001:73`); `integration UNIQUE(tenant_id, source)` (`0001:76`); `source` enum sem scraper
  (`entity.go:16`). `court_case.assigned_user_id` **existe** (migration 0029, FK `app_user`). Zero
  groundwork de headless/RPA/parser-HTML/crypto/vault (grep). DJEN connector **já usa uTLS Chrome + proxy
  CONNECT** contra WAF (`djen.go:16,183-192`). 96,4% do caseload é TJSP, 93% JEC Franca/Ribeirão; TJSP
  migrou eSAJ→eproc a partir de mar/2025 (JEC).

**Decisões em aberto (não bloqueantes, voltam ao produto):**
- Fallback "credencial padrão do escritório" para processos sem responsável (§4.3) — recomendo **não** no
  v0 (usar senha alheia em processo que não é dele é sensível).
- Cadência default de re-sync do scraper (§10) — **RECOMMENDATION:** bem mais espaçada que o DJEN (ex.:
  diária/sob-demanda, não horária), com jitter; comunicar "atraso aceitável" na UI ("sincronizado há X").
- Senha dedicada × pessoal (§6) — oferecer na UI como recomendação.
- Refresh do `eproc_coverage` a cada trimestre (manual vs. pipeline).

---

## 12. Ordem de implementação (fatias verticais)

Cada fatia = slice pequeno, verde, `pm-plan → dev-qa (TDD) → code-review → merge`. A ordem **desrisca os
UNKNOWNs cedo** (o que o portal expõe + se HTTP puro basta) e adia o hard (cofre) para depois de provado o
login+parse — mesma filosofia do ERD MNI.

1. **`lib/scraper` — login + sessão + consulta + parse contra o portal real (a fatia que desrisca tudo).**
   Reusa o transporte uTLS+proxy do DJEN; implementa `PortalSession` (login → cookie → consulta) e o parser
   HTML → `ParsedResult` (parties com `Document`, docket, documents[]). **Valida os dois UNKNOWNs maiores:**
   (a) HTTP+HTML basta ou precisa headless? (b) o que o eproc expõe de CPF/CNPJ com/sem procuração + cai em
   captcha? Contra conta real de JEC de Franca/Ribeirão (ACCEPTANCE — dado real). *(Primeira fatia — responde
   as perguntas que mudam o critério de aceite; nada depende do cofre para isso — usar credencial de teste
   por env var/local só nesta fatia.)*
2. **Cofre de credencial + `portal_credential` + fluxo de configuração POR-ADVOGADO.** `tenant_secret`
   cifrado (envelope encryption); tabela `portal_credential`; `POST` no Painel de Integrações onde **o
   advogado** informa login/senha do portal → `portal_credential` + `credential_ref` + `status`. Onboarding
   com o **texto de proteção/limite de uso antes do campo de senha** (ACCEPTANCE #5). Porta
   `PortalCredentialProvider.Credentials(...)`.
3. **Conector scraper real registrado no `Orchestrator`** + sync por evento com **escolha de credencial via
   `court_case.assigned_user_id`** + **elegibilidade `eproc_coverage`** + **cadência conservadora /
   serialização por advogado**: `Fetch` autenticado → `Parse` → upsert `party.document`/`source=SCRAPER` em
   tx+outbox. **Taxonomia de erro** (§8) categorizando cada `sync_run`/`portal_credential.status`
   (ACCEPTANCE #2, #4). Round-trip produtor∥consumidor.
4. **`eproc_coverage`** (seed 6ª RAJ) + predicado `eligibleForScraper` no sync — corta eSAJ residual e
   processos sem responsável/credencial antes da rede (ACCEPTANCE #3). *(Pode fundir com a fatia 3 se
   pequena.)*
5. **`DocumentSource` adapter scraper** — baixa inteiro teor → `document.origin=COURT` → pipeline de
   extração/chunk (`erd-documentos.md`).
6. **UI no Cockpit** — "última sincronização bem-sucedida" + fonte discreta **por seção** (Partes/
   Andamentos/Documentos), **sem** selo comparativo (edge #5); sinal acionável de credencial rejeitada
   (`CREDENTIAL_INVALID`) e degradação por captcha recorrente (`CAPTCHA_BLOCKED`) (ACCEPTANCE #4).
7. **(Futuro, fora do v0)** peticionamento: `FilingGateway`/`Signer` (`erd-pecas.md`) — **sempre com
   aprovação humana** (advisory, inegociável).
