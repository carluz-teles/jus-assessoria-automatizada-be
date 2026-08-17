# ERD — PJe/MNI (consulta direta ao tribunal, read-only)

> **Status:** desenho (v0 → habilita **partes reais (CPF/CNPJ)**, **autos oficiais** e **andamentos**
> no Cockpit do Processo, consultando o tribunal pelo protocolo **MNI/CNJ-padrão** — começando por
> **TJSP eproc** (Juizado Especial Cível). É o **primeiro produtor real** de `party.document` e de
> `document.origin=COURT`. **100% leitura nesta fatia** — nenhuma escrita no tribunal, nenhum
> peticionamento (isso reusa `FilingGateway`/`Signer` de `erd-pecas.md`, fatia futura).
> **Fonte de verdade do schema:** `erd-modelo-de-dados.md` (`integration`, `party`, `party_counsel`,
> `document`, `court_record`, `docket_entry`). Onde divergir, o schema vence. Complementa
> `erd-documentos.md` (a porta `DocumentSource` que aqui recebe seu primeiro adapter), `erd-pecas.md`
> (as portas `FilingGateway`/`Signer` que **não** implementamos aqui) e o slice `internal/acquisition`
> (o conector MNI é mais um `source`, no mesmo molde de DJEN/DATAJUD).

---

## 1. Contexto & objetivo

Hoje o dado de processo nasce do **DJEN** (descobre por OAB) e do **DATAJUD** (enriquece por número).
Nenhum dos dois entrega o que o advogado mais precisa para trabalhar o caso: **DJEN nunca revela
CPF/CNPJ das partes** (LGPD — `party.document` é sempre NULL, `internal/acquisition/connector.go:104`),
**DATAJUD não entrega documentos dos autos** (`erd-documentos.md §5`), e ambos são *observação passiva
do diário*, não a consulta ao processo. O `document.origin=COURT` ("baixado dos autos, via MNI/A1")
está **nomeado mas nunca populado** (`internal/document/entity.go:55`); Documentos v0 vive só de UPLOAD.

**Objetivo:** um conector **`source=MNI`** dentro de `internal/acquisition` (mesmo `Connector`/`Parser`/
`Orchestrator` de DJEN/DATAJUD) que, para `court_record` **elegível** (TJSP, eproc-nativo), chama a
operação **`consultarProcesso`** do MNI e traz: **partes com documento (CPF/CNPJ quando o tribunal
expõe)**, **andamentos** e **metadados dos autos** (a lista de documentos e o download do inteiro teor,
alimentando `document.origin=COURT` via a porta `DocumentSource` de `erd-documentos.md`). A consulta é
**autenticada por certificado ICP-Brasil por-tenant**, configurado explicitamente pelo escritório; uma
vez configurada, a consulta é **automática por-processo elegível** (mesma filosofia dos re-polls de
`scheduler.go`). É o subsistema que desfaz o corte deliberado de v0 anterior — *"o Secrets Manager
pesado do MNI está cortado"* (`docs/fundacao-prd-erd.md:71`).

**O que este slice NÃO faz (v0):** escrever no tribunal, peticionar, assinar peça, ou consultar
qualquer tribunal além de **TJSP eproc**. Peticionamento continua sendo `FilingGateway`/`Signer`
(`erd-pecas.md`) e permanece sujeito ao advisory *"IA nunca protocola sozinha, sempre aprovação
humana"* — **inegociável mesmo fora do v0** (CONSTRAINT do PM).

---

## 2. Reuse-check (Regra nº1)

| Procurei por | Achei | Decisão |
|---|---|---|
| conector de fonte externa | `internal/acquisition/connector.go`: `Connector` (`ID`,`Version`,`Capabilities`,`Fetch(ctx,FetchRequest)→RawPayload`), `Parser` (`CanParse`,`Parse→ParsedResult`); `orchestrator.go` (`Register(source,c)`,`ConnectorFor(source)`) | **EXTEND** — MNI é mais um `source`; **não** criar slice novo nem porta nova de conector |
| enum `source` + `integration` | `migrations/0001` `integration(tenant_id, source)` UNIQUE, `credential_ref text` ("pointer to vault; null in v0"), `status ...AUTH_FAILED...`; comentário já reserva `MNI` | **REUSE** — a linha `integration` já é o registro do conector; `MNI` entra como constante nova em `entity.go:17` |
| `FetchRequest` sem credencial | `connector.go:42` — *"carries no credentials — the connector resolves those from its own configuration (credential_ref)"* | **REUSE** — o conector MNI resolve o certificado do cofre atrás de `credential_ref`, igual ao previsto |
| resultado agnóstico de fonte | `ParsedResult{CourtRecords, DocketEntries, Intimations, Parties}`; `ParsedParty` já tem `Role/Name/Counsels` | **EXTEND** — só falta **preencher `document` da parte** (novo campo em `ParsedParty`) e a **lista de documentos dos autos** (novo campo) |
| gatilho por evento | `listener.go:68-70` consome `TypeIntegrationActivated`/`TypeSyncRequested`/`TypeCourtRecordObserved`; `scheduler.go` re-poll por `court_record.next_sync_at` | **REUSE** — o sync MNI é disparado pelos MESMOS eventos; nenhum trigger novo |
| partes reais | `party.document` (0032) sempre NULL; `party.source` já aceita `MANUAL`, reservável p/ `MNI` | **EXTEND** — MNI é o **1º produtor** de `party.document`; `source` ganha valor `MNI` |
| documento dos autos | `document.origin=COURT` (`internal/document/entity.go:55`) nomeado, nunca populado; porta `DocumentSource` desenhada em `erd-documentos.md §5` mas sem adapter | **EXTEND** — o MNI é o **1º adapter de `DocumentSource`** e o 1º produtor de `origin=COURT` |
| cofre de segredo por-tenant | ⚠️ **nada**: `lib/config` (`caarlos0/env/v11`) só env var process-wide; `grep` por kms/vault/pgcrypto-encrypt/pkcs12 → **zero groundwork** | **CREATE** — `credential_ref` ganha seu 1º produtor; precisa de um cofre (§6) |
| cliente SOAP / assinatura XML / certificado | ⚠️ **nada**: sem SOAP/WSDL, sem XMLDSig, sem manejo de PKCS12/x509 no `go.mod`; MNI é **SOAP**, não REST/JSON como DJEN/DATAJUD | **CREATE** — `lib/mni` (cliente SOAP + WS-Security por certificado) é o maior trabalho novo (§5, §7) |
| peticionamento / assinatura | portas `FilingGateway`/`Signer` **nomeadas** em `erd-pecas.md`, sem impl | **PRESERVE** — fora do v0; não implementar, mas o cofre/certificado desenhados aqui são o insumo delas depois |

Conclusão: o **domínio de aquisição está pronto para receber MNI** (conector/parser/orchestrator/eventos
não mudam de forma). O trabalho real e **novo** é: (a) `lib/mni` — cliente **SOAP** com autenticação por
**certificado ICP-Brasil** (zero groundwork hoje); (b) o **cofre de credencial por-tenant** por trás de
`credential_ref` (também zero); (c) o **mapa de elegibilidade eproc** (estoque residual eSAJ vs. nativo);
(d) preencher `party.document` e `origin=COURT` (deltas mínimos em `ParsedParty` + adapter `DocumentSource`).

---

## 3. Princípios (decididos)

1. **MNI é um `source`, não um domínio novo.** O conector/parser/orchestrator/eventos de `acquisition`
   já são a forma certa (`FetchRequest` sem credencial, `ParsedResult` agnóstico). Introduzir um slice
   paralelo seria duplicar o pipeline de sync — **EXTEND, nunca INTRODUCE** (a skill manda preferir isso).
2. **100% leitura no v0.** `consultarProcesso` só lê. Escrita no tribunal (`entregarManifestacaoProcessual`
   do MNI) é `FilingGateway` (`erd-pecas.md`), fatia futura, sujeita à aprovação humana obrigatória.
3. **Credencial explícita, consulta automática.** O escritório **configura** o certificado (ação humana,
   uma vez); a **consulta** por processo elegível é automática (mesma filosofia de re-poll do DJEN/DATAJUD).
   Nunca ligar MNI sozinho sem o escritório entregar a credencial.
4. **Elegibilidade antes de rede.** Nenhuma tentativa MNI contra processo que ainda está em **eSAJ
   residual**. A decisão (eproc-nativo × eSAJ) é resolvida **localmente** (mapa + heurística de data),
   antes do `Fetch` — evita ruído de falha constante (ACCEPTANCE nº3).
5. **Certificado nunca em claro.** `credential_ref` aponta para um cofre; o segredo nunca aparece em
   código, log, outbox, trace ou `scope`. Custódia de certificado ICP-Brasil é a **identidade digital do
   advogado** — risco jurídico de primeira grandeza (§10).
6. **Origem importa juridicamente.** `document.origin=COURT` (dos autos, via MNI) tem peso probatório que
   UPLOAD não tem, e `party.source=MNI` com `document` preenchido é o dado *confiável* que o produto
   promete. O Cockpit **sinaliza a fonte** (MNI × DJEN × UPLOAD) quando relevante.
7. **Degradação honesta.** Falha de consulta **nunca quebra a tela** — degrada com mensagem
   **categorizada** (indisponível / não encontrado / credencial inválida), e a última fonte boa
   (DJEN/DATAJUD) continua valendo (ACCEPTANCE nº2).
8. **Cobertura cresce com o tempo.** O rollout do eproc é progressivo por região (~4 anos); o mapa de
   elegibilidade é **dado versionado**, não código — cresce sem deploy do motor.

---

## 4. Modelo de dados (referência ao catálogo)

**Decisão de modelagem:** **quase nenhum DDL estrutural novo** — `integration`/`party`/`document` já
servem. O único DDL **novo** é o **mapa de elegibilidade eproc** e (§6) a **coluna cifrada do cofre**.

- **`integration`** (existe, 0001) — a linha `(tenant_id, source='MNI')` **é** o registro do conector MNI.
  - `scope jsonb` — hoje `{oab, taxId}`; MNI **não descobre por OAB** (consulta por número), então o
    scope pode ficar vazio/mínimo; o que importa é `credential_ref`.
  - `credential_ref text` — **ganha seu 1º produtor real** (aponta pro segredo cifrado; §6). Continua
    NULL para DJEN/DATAJUD.
  - `status` — reusa o enum existente: `AUTH_FAILED` passa a ser **de fato escrito** quando o certificado
    expira/é revogado (hoje só reservado); alimenta a sinalização acionável (edge case do PM).
  - **Delta proposto (a confirmar no catálogo):** `credential_meta jsonb` (nullable) — metadados **não
    secretos** do certificado para a UI acionável: `{subject_cn, issuer, not_after, fingerprint}`. Deixa
    o Cockpit avisar "certificado vence em N dias / expirado" **sem** tocar o segredo. Nada de chave aqui.
- **`party`** / **`party_counsel`** (0032) — MNI é o **1º produtor de `party.document`**. Sem DDL: só
  passa a **escrever** o `document` (CPF/CNPJ) e `source='MNI'` (a UNIQUE `(tenant, case, role, name)`
  faz o upsert idempotente — MNI **enriquece** a mesma parte que o DJEN já criou pelo nome, preenchendo
  o documento antes NULL). Ordem de precedência: MNI (documento real) **sobrepõe** a linha do DJEN.
- **`document`** / **`chunk`** (0001) — MNI é o **1º produtor de `origin=COURT`**. Sem DDL estrutural:
  o adapter `DocumentSource` (de `erd-documentos.md`) cria o `document` com `origin=COURT`,
  `court_record_id` preenchido, e dispara o pipeline de extração/chunk já existente. O byte do inteiro
  teor entra no storage (`lib/storage`), não passa pela API.
- **`court_record`** (0001) — sem DDL novo. A **elegibilidade** é resolvida por (a) o **mapa novo** abaixo
  e (b) sinais que já existem na linha: `court` (= `TJSP`), `cnj_number` (o código CNJ `.8.26.` = TJSP e
  a comarca/vara embutidos), `filed_at` (data de ajuizamento — pré/pós migração da comarca).
- **`eproc_coverage`** (**DDL novo** — o único estrutural) — o mapa estático de cobertura eproc por
  comarca/vara do TJSP, para o edge case de estoque residual. Referência global (não por-tenant), semeado
  por migration e refinado sob demanda (mesma filosofia do seed estadual de feriados de `erd-prazos.md`):
  ```sql
  -- eproc_coverage — quando cada comarca/vara do TJSP passou a nascer nativa em eproc.
  -- Referência GLOBAL (não tem tenant_id, não tem RLS) — é fato público de rollout do TJSP.
  -- A elegibilidade de um court_record: court='TJSP' AND filed_at >= migrated_at do seu foro.
  CREATE TABLE eproc_coverage (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    court        text NOT NULL,          -- 'TJSP' (aberto p/ outros tribunais no futuro)
    raj          text,                   -- Região Administrativa Judiciária (ex.: '6') — informativo
    comarca_code text NOT NULL,          -- código CNJ da comarca/foro (origem do cnj_number)
    system       text NOT NULL DEFAULT 'EPROC',  -- EPROC | ESAJ (o que passou a nascer ali)
    migrated_at  date NOT NULL,          -- a partir de quando processos nascem nativos nesse system
    note         text,
    UNIQUE (court, comarca_code, system)
  );
  CREATE INDEX ON eproc_coverage (court, comarca_code);
  ```
  > **Por que uma tabela e não um arquivo em `/lib`:** é **dado que muda com o rollout** (novas comarcas
  > migram a cada trimestre), consultado por-`court_record` no caminho quente de elegibilidade, e o produto
  > vai querer editá-lo sem deploy — exatamente o critério que `erd-prazos.md §11` usou para preferir a
  > tabela seed `deadline_rule` a um arquivo. **Concordo com a recomendação do PM (mapa estático) e a
  > materializo como tabela seed**, não como constante Go.

---

## 5. Integrações necessárias

| Integração | Papel | Porta / estado |
|---|---|---|
| **MNI (SOAP/WSDL)** do TJSP eproc | `consultarProcesso` → partes, andamentos, docs; `consultarAvisosPendentes`/`consultarTeorComunicacao` (futuro) | 🔴 **novo/hard** — `lib/mni`: cliente **SOAP** (não há no repo), request/response por WSDL do MNI 2.2.2 do CNJ; **zero groundwork** (`go.mod` sem SOAP) |
| **Autenticação MNI (ICP-Brasil)** | assinar/autenticar a chamada SOAP com o certificado do advogado | 🔴 **novo/hard** — porta `MNICredentialProvider` (§7); v0 recomendado **certificado A1 (.pfx)** custodiado no cofre **ou** assinador em nuvem (BirdID/VIDaaS/Serpro) via API — **a porta abstrai as duas**; o fornecedor fica **em aberto** (§10) |
| **Cofre de credencial por-tenant** | onde mora o certificado/segredo cifrado atrás de `credential_ref` | 🔴 **novo** — sem KMS/Vault na infra hoje (Railway+Postgres); v0 recomendado **coluna cifrada no Postgres com envelope encryption** por chave de app (§6) |
| **`DocumentSource`** (`erd-documentos.md`) | baixar o inteiro teor dos autos → `document.origin=COURT` | 🟡 porta desenhada, **1º adapter aqui** (MNI); reusa `lib/storage` + o pipeline de extração/chunk |
| **`internal/acquisition`** (conector/parser/orchestrator/sync/scheduler) | o pipeline que já roda DJEN/DATAJUD | ✅ existe — só **registrar** o conector/parser MNI (`worker-ingestao/main.go`) |
| **`eproc_coverage`** (mapa) | decidir elegibilidade sem rede | 🟡 **novo** — tabela seed (§4) |
| **Painel de Integrações (FE)** | onde o escritório sobe/gere o certificado | 🟡 já antecipado no FE (`src/features/integrations/`: *"Fontes futuras exigirão credencial/login"*) — a casa natural da configuração |

**MNI é SOAP, não REST/JSON.** O `RawPayload{Body []byte}` **basta** (é bytes opacos — o XML de resposta
cabe nele igual ao JSON do DJEN). O `Parser.CanParse` casa por `p.Source == SourceMNI` (igual
`djen_parser.go:104`). **Nenhuma mudança em `RawPayload`/`Connector`/`Parser`.** O único delta de
contrato é em `ParsedResult`/`ParsedParty` (§8, campo `Document` + lista de documentos dos autos).

---

## 6. Cofre de segredos por-tenant (`credential_ref`)

**Problema:** `credential_ref` nunca teve produtor; `lib/config` só faz env var process-wide — **não
serve** para um segredo **por-tenant** (o certificado de cada escritório). Não há KMS/Vault na infra
conhecida (Railway + Postgres; `grep` confirma zero menção de AWS KMS/HashiCorp Vault em `lib`/docs).

**Opções avaliadas:**

- **OPÇÃO A — Coluna cifrada no Postgres, envelope encryption com chave de app (recomendado v0).**
  O certificado (`.pfx`) e sua senha ficam cifrados numa tabela `tenant_secret` (DEK por-segredo, DEK
  cifrada por uma KEK vinda de env var — `caarlos0/env`, `required`); `credential_ref` aponta a linha.
  - **Prós:** sem infra nova (só Postgres, que já temos); rotação de KEK viável; segredo nunca em claro
    no banco (mesmo com dump); alinha ao `credential_ref` já desenhado; RLS isola por tenant.
  - **Contras:** a KEK é um env var (mesma superfície do resto dos segredos, `fundacao-prd:N9`); não é
    HSM. Aceitável para v0 dado o estágio — **mas** o material custodiado é sensível (§10).
- **OPÇÃO B — Secrets manager dedicado (Vault/AWS Secrets Manager/KMS).**
  - **Prós:** custódia de nível industrial, auditoria, HSM opcional (ideal para certificado ICP-Brasil).
  - **Contras:** **infra nova** que não existe hoje na Railway; custo/operação; é o *"Secrets Manager
    pesado do MNI"* que a v0 anterior **cortou de propósito** (`fundacao-prd:71`). Introduzir agora seria
    contrariar a skill (não introduzir infra por escalabilidade teórica).
- **OPÇÃO C — Assinador em nuvem, o segredo nunca toca nossa infra.**
  O escritório cadastra credenciais de um provedor (BirdID/VIDaaS/Serpro); guardamos só o **token/refresh
  do provedor** (segredo bem menor), e a assinatura da chamada MNI acontece no provedor.
  - **Prós:** **nunca custodiamos o `.pfx`** — reduz drasticamente o risco jurídico do §10.
  - **Contras:** depende do produto decidir o fornecedor (ambiguidade nº3 do PM, **em aberto**); ainda
    exige o cofre (A) para o token do provedor.

**DECISION:** **Cofre = Opção A** (coluna cifrada, envelope encryption, KEK em env var) como mecanismo
de custódia; **a natureza do que se custodia** (.pfx próprio × token de assinador em nuvem) é decidida
pela **porta** `MNICredentialProvider` (§7), que funciona com **A ou C** sem retrabalho de cofre. **A
Opção B fica explicitamente adiada** (não reintroduzir o secrets manager pesado no v0). **Se** o produto
escolher assinador em nuvem (C), o material custodiado encolhe (token, não certificado) — e o §10 fica
muito mais leve. Esse é o caminho que eu **recomendo** ao dono do produto.

---

## 7. Portas / interfaces (o que muda no código)

Duas portas novas + reuso das existentes. Nomes propostos (DEV ajusta):

- **`MNICredentialProvider`** (porta nova, em `lib/mni` ou no conector) — abstrai **como a chamada SOAP é
  autenticada/assinada**, resolvendo o material a partir de `credential_ref`:
  ```
  // Assina/autentica uma requisição MNI para um tenant, sem expor o material.
  // A impl A1 lê o .pfx cifrado do cofre e assina localmente (WS-Security);
  // a impl "nuvem" delega ao provedor (BirdID/VIDaaS/Serpro) por API.
  Sign(ctx, tenantID, credentialRef, soapEnvelope []byte) (signed []byte, err error)
  ```
  Isto **não trava** o fornecedor (ambiguidade nº3): A1 local e assinador em nuvem satisfazem a mesma
  porta. Erro de credencial (expirada/revogada) é **tipado** (`apperr` `AUTH_FAILED`) e sobe até virar
  `integration.status=AUTH_FAILED` + sinal na UI.
- **`DocumentSource`** (porta **já nomeada** em `erd-documentos.md §5`) — **1º adapter aqui**: dado um
  `court_record` + a resposta MNI, baixa o inteiro teor de cada documento dos autos e cria `document`
  com `origin=COURT`. Reusa `lib/storage` + o pipeline de extração/chunk existente.
- **`Connector` MNI** (impl da porta existente `connector.go`) — `ID()="mni"`, `Version()`,
  `Capabilities()=[FETCH_BY_NUMBER]` (**não** `DISCOVER_BY_OAB` — MNI consulta por número), `Fetch` monta
  o envelope `consultarProcesso`, chama `MNICredentialProvider.Sign`, faz o POST SOAP, devolve
  `RawPayload{Source: SourceMNI, Body: xml}`. Registrado no `Orchestrator` (`worker-ingestao/main.go`).
- **`Parser` MNI** (impl da porta existente) — `CanParse` casa `Source==SourceMNI`; `Parse` faz o
  unmarshal do XML MNI → `ParsedResult` (records, docket entries, **parties com `Document`**, e a lista
  de documentos dos autos para o `DocumentSource`).
- **`Capabilities` enum** — **não precisa de valor novo**: `FETCH_BY_NUMBER` já existe (`connector.go:23`).
- **`FetchRequest`/`RawPayload`** — **não mudam** (§5). `FetchRequest` já carrega `CNJNumber`/`Court`/
  `IntegrationID` e nenhuma credencial (por design).

**Delta de contrato (o único):** `ParsedParty.Document string` (CPF/CNPJ, vazio quando o tribunal não
expõe — ver UNKNOWN §11) e um `ParsedResult.Documents []ParsedDocument` (metadados dos autos para o
`DocumentSource`). Round-trip produtor∥consumidor obrigatório ([[parallel-producer-consumer-roundtrip]]).

---

## 8. Descoberta de elegibilidade (estoque residual eSAJ × eproc nativo)

O critério, resolvido **localmente antes de qualquer rede** (ACCEPTANCE nº3):

```
elegível_para_MNI(court_record) :=
     court_record.court == 'TJSP'
 AND comarca(court_record.cnj_number) tem linha em eproc_coverage (system='EPROC')
 AND court_record.filed_at >= eproc_coverage.migrated_at da sua comarca
 AND integration(tenant, 'MNI').status == ACTIVE   (certificado configurado e válido)
```

- **`court`** já está na linha; **comarca** sai do `cnj_number` (o código CNJ carrega tribunal `.8.26.` +
  origem/foro). **`filed_at`** (ajuizamento, hoje só do DATAJUD) é o discriminador pré/pós-migração:
  processo **nascido depois** da migração da comarca é eproc-nativo; **antes** é estoque residual eSAJ →
  **não tenta MNI**.
- **Quando `filed_at` está ausente** (DJEN puro, sem enriquecimento DATAJUD): viés **conservador** —
  **não** tentar MNI (evita ruído), e opcionalmente sinalizar "elegibilidade indeterminada, enriqueça
  por DATAJUD". Nunca chutar elegível.
- **Onde a decisão roda:** no `sync` do conector, **antes** do `Fetch` MNI — o listener/scheduler já
  entrega o `court_record`; um predicado `eligibleForMNI` curto-circuita e o run nem sai para a rede.
  Isso mantém o `scheduler.go` (re-poll por `next_sync_at`) sem tentativas falhas repetidas contra eSAJ.

---

## 9. Arquitetura / pipeline

```mermaid
sequenceDiagram
  participant FE as FE (Integrações / Cockpit)
  participant API as cmd/api (internal/acquisition + cofre)
  participant VLT as tenant_secret (cofre cifrado)
  participant SCH as scheduler / listener
  participant MNIC as conector MNI (lib/mni, SOAP)
  participant CP as MNICredentialProvider (A1/nuvem)
  participant TRIB as TJSP eproc (MNI SOAP)
  participant P as Parser MNI
  participant DS as DocumentSource → lib/storage
  participant PG as Postgres (party/document/docket)

  FE->>API: configura certificado (upload .pfx / credencial nuvem)
  API->>VLT: cifra + grava; integration(MNI).credential_ref, status=ACTIVE, credential_meta{not_after}
  Note over SCH: court_record elegível (§8) + integration MNI ACTIVE
  SCH->>MNIC: Fetch(FETCH_BY_NUMBER, cnj, court=TJSP)  [só se eligibleForMNI]
  MNIC->>CP: Sign(tenant, credential_ref, envelope consultarProcesso)
  CP->>VLT: decifra material (A1) — ou delega (nuvem)
  CP-->>MNIC: envelope assinado
  MNIC->>TRIB: POST SOAP consultarProcesso
  TRIB-->>MNIC: XML (partes+CPF/CNPJ, andamentos, docs)
  MNIC-->>P: RawPayload{Source=MNI, Body=xml}
  P->>P: Parse → ParsedResult (parties.Document, docket, documents[])
  P->>PG: upsert party.document + party.source=MNI [tx + outbox]
  P->>DS: baixa inteiro teor → document.origin=COURT → pipeline extração/chunk
  MNIC-->>SCH: falha? run FAILED categorizado (indisponível/não-encontrado/auth) — acka, não quebra
  TRIB-->>MNIC: 401/cert inválido → integration.status=AUTH_FAILED → sinal acionável no FE
```

Escrita sempre em tx + outbox; cada etapa idempotente (`processed_event`). A **falha** de MNI vira um
`sync_run` FAILED **categorizado** e o conector acka (mesma política de `connector.go:70` — o scheduler
re-tenta depois), **sem** burnar retries do asynq e **sem** quebrar a tela.

---

## 10. Riscos de segurança (custódia de certificado ICP-Brasil)

**Risco central:** o certificado A1 é a **identidade digital do advogado** — com ele, pode-se **assinar e
peticionar em nome dele**. Custodiar `.pfx` é responsabilidade jurídica não-trivial: um vazamento não é
"leak de dado", é **impersonação de um advogado perante o Judiciário**.

| Risco | Mitigação proposta |
|---|---|
| Vazamento do `.pfx` custodiado | **Preferir Opção C (assinador em nuvem)** — nunca custodiar o `.pfx`; se A1, envelope encryption (DEK/KEK, §6), KEK só em env var, nunca no dump; RLS por tenant; **jamais** em log/outbox/trace/`scope` |
| Uso indevido além de leitura | v0 é **read-only**; a porta só assina `consultarProcesso`; qualquer escrita exige `FilingGateway` + **aprovação humana** (advisory, inegociável) |
| Certificado expirado/revogado | `credential_meta.not_after` → aviso proativo no Cockpit **antes** de vencer; erro de auth → `AUTH_FAILED` + sinal **acionável** (edge case do PM) |
| Consentimento/escopo | o escritório **configura explicitamente** (nunca automático); registrar quando/quem configurou (auditoria) |
| KEK única comprometida | rotação de KEK planejada; DEK por-segredo limita blast radius; migração para KMS (Opção B) é caminho de saída sem mudar `credential_ref` |

**Recomendação forte ao produto:** ir de **assinador em nuvem (C)** justamente para **não** custodiar a
identidade digital do advogado — transforma o maior risco jurídico do subsistema num problema de token.

---

## 11. Pontos de falha & decisões (marcadas FACT/ASSUMPTION/UNKNOWN/DECISION)

**DECISION (travadas neste desenho):**
- ✅ MNI é `source=MNI` **dentro de `internal/acquisition`** (EXTEND do conector/parser), não slice novo.
- ✅ `RawPayload`/`FetchRequest`/`Connector`/`Parser` **não mudam**; SOAP cabe em `Body []byte`. Único
  delta de contrato: `ParsedParty.Document` + `ParsedResult.Documents[]`.
- ✅ Elegibilidade resolvida **localmente** por `eproc_coverage` (tabela seed) + `filed_at` ≥ migração,
  **antes** da rede — sem ruído contra eSAJ residual. Concordo com o "mapa estático" do PM, como tabela.
- ✅ Cofre = **coluna cifrada no Postgres (envelope encryption)**, KEK em env var; **secrets manager
  pesado adiado** (mantém o corte de `fundacao-prd:71` para a mecânica, desfaz só o produto MNI).
- ✅ Custódia via porta `MNICredentialProvider` **agnóstica de fornecedor** (A1 local × nuvem).
- ✅ v0 **100% leitura**; peticionamento reusa `FilingGateway`/`Signer` (`erd-pecas.md`), fora do escopo.

**ASSUMPTION (assumido, a validar):**
- Que o eproc do TJSP fala **MNI 2.2.2 (CNJ-padrão, SOAP/WSDL)** de forma consultável por advogado
  autenticado — o eproc é compatível com MNI, mas a *versão/endpoint exatos do TJSP* precisam de
  confirmação na doc/sandbox.
- Que `filed_at` (ajuizamento) discrimina bem eSAJ residual × eproc nativo por comarca. Amostra real de
  Franca/Ribeirão Preto (6ª RAJ) valida (ACCEPTANCE nº5).
- Que a UNIQUE `(tenant, case, role, name)` de `party` casa a parte do DJEN com a do MNI pelo nome para
  enriquecer o `document` — colisão/variação de grafia de nome é risco a medir.

**UNKNOWN (crítico, bloqueia critério de aceite — validar contra WSDL real antes de implementar):**
- ⚠️ **O maior:** *o que `consultarProcesso` expõe de **CPF/CNPJ** para um advogado autenticado **sem
  procuração** nos autos vs. **com** procuração?* O MNI/eproc pode **mascarar** o documento (ou o
  processo inteiro, por sigilo/segredo de justiça) para quem não é parte/procurador. **Isto muda o
  ACCEPTANCE nº1** ("retorna CPF/CNPJ quando exposto"). **Não assumir otimisticamente que vem tudo
  aberto.** Precisa de validação no **schema WSDL real + resposta de sandbox/amostra** antes da fatia de
  parsing de partes. Se vier mascarado sem procuração, o valor de "partes reais" fica condicionado a ter
  procuração cadastrada — decisão de produto a reabrir.
- ⚠️ **Fornecedor de credencial (ambiguidade nº3 do PM):** A1 (.pfx) custodiado × assinador em nuvem
  (BirdID/VIDaaS/Serpro) — **volta pro dono do produto**. O desenho não trava (porta), mas a decisão
  muda drasticamente o §10.

**FACT (evidência de código/negócio já levantada):**
- `party.document` sempre NULL, DJEN nunca revela (`connector.go:104`, migration 0032). `document.origin=
  COURT` nomeado, nunca populado (`entity.go:55`). `integration.credential_ref` sem produtor, "null in
  v0" (0001:44). `source enum` reserva MNI (0001:42). Zero groundwork SOAP/cripto/vault (grep). 96,4% do
  caseload é TJSP, 93% JEC em Franca/Ribeirão; TJSP migrou eSAJ→eproc a partir de mar/2025; 6ª RAJ entre
  jun–set/2025.

**Decisões em aberto (não bloqueantes):**
- Refresh do `eproc_coverage` (re-rodar seed a cada trimestre de rollout) manual vs. pipeline.
- Precedência fina quando MNI e DJEN discordam de um nome de parte (merge vs. duplicar).
- Cadência de re-consulta MNI (mais cara que DJEN) — `next_sync_at` mais espaçado para `source=MNI`.

---

## 12. Ordem de implementação (fatias verticais)

Cada fatia = slice pequeno, verde, `pm-plan → dev-qa (TDD) → code-review → merge`. A ordem **desrisca o
UNKNOWN cedo** e adia o hard (cofre/custódia) para depois de provado o parsing.

1. **`lib/mni` — cliente SOAP + parse de `consultarProcesso` contra amostra/sandbox.** Sem cofre, sem
   rede autenticada real: monta o envelope, faz o unmarshal do XML MNI → `ParsedResult` (parties com
   `Document`, docket, documents[]). **Aqui se resolve o UNKNOWN nº1** (o que o schema expõe de CPF/CNPJ):
   validar contra WSDL + payload real de JEC de Franca/Ribeirão (ACCEPTANCE nº5). *(Primeira fatia — é a
   que responde a pergunta que muda o critério de aceite; nada depende do cofre para isso.)*
2. **Cofre de credencial por-tenant** (`tenant_secret` cifrado, envelope encryption) + fluxo de
   **configuração de certificado** (`POST` no painel de Integrações) → `integration(MNI).credential_ref`,
   `status`, `credential_meta`. Porta `MNICredentialProvider` (impl A1 primeiro).
3. **Conector MNI real registrado no `Orchestrator`** + sync por evento: `Fetch` autenticado (via cofre)
   → `Parse` → upsert `party.document`/`source=MNI` em tx+outbox. Falha **categorizada** (ACCEPTANCE nº2).
   Round-trip produtor∥consumidor.
4. **Mapa de elegibilidade `eproc_coverage`** (seed 6ª RAJ) + predicado `eligibleForMNI` no sync — corta
   eSAJ residual antes da rede (ACCEPTANCE nº3).
5. **`DocumentSource` adapter MNI** — baixa inteiro teor → `document.origin=COURT` → pipeline de
   extração/chunk (`erd-documentos.md`).
6. **UI no Cockpit** — indicador de **fonte do dado** (MNI × DJEN × UPLOAD) na aba Partes/Documentos +
   sinal acionável de **certificado expirado/revogado** (`AUTH_FAILED`/`credential_meta.not_after`).
7. **(Futuro, fora do v0)** peticionamento: `FilingGateway`/`Signer` (`erd-pecas.md`) reusando o cofre e a
   `MNICredentialProvider` — **sempre com aprovação humana** (advisory, inegociável).
