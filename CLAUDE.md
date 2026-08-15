# jus-assessoria — Backend (v0 · fundação)

Plataforma de assessoria jurídica automatizada. Este repo é o **backend Go**. Fonte de verdade do design:
`docs/erd-backend.md` (arquitetura), `docs/erd-modelo-de-dados.md` (schema, **fonte de verdade do banco**),
`docs/fundacao-prd-erd.md` (PRD da plataforma). **Onde este arquivo e os docs divergirem, os docs vencem.**

Módulo Go: `github.com/jusassessoria/platform` · Go 1.25.

## Regra nº1 (inegociável)
Antes de escrever QUALQUER código, procure o que já existe por **responsabilidade** (grep por conceito/colunas/fórmula,
não só pelo nome). Declare no output um bloco **Reuse-check**: (a) o que procurou, (b) o que achou, (c) decisão
REUSE / EXTEND / CREATE com o porquê. Lógica duplicada é bug de design (uma só fonte de verdade).

## Arquitetura — Vertical Slice
```
/cmd        um binário por pasta (api, worker-ingestao, worker-documents, worker-ai, worker-outbox-relay, scheduler)
/internal   os slices — cada entidade tem TUDO dentro de si
/lib        infra compartilhada (não é domínio), injetada nos slices
/migrations SQL versionado (golang-migrate), roda no boot só do api
/pkg        utilitários genéricos, sem regra de negócio
/infra      provisionamento Railway via API GraphQL (infra/railway/*.sh) — não é mais Terraform
```
Anatomia de um slice (`internal/<slice>`): `entity.go` `errors.go` `validation.go` `mapper.go` `domain.go`
`handler.go` `listener.go` `repository.go` `events.go` `/queries/*.sql` `*_test.go`.

Regra de dependência DENTRO do slice: `handler|listener → domain(caso de uso) → entity ← repository(impl sqlc)`.
`entity.go` não importa repository/handler/lib. `domain.go` depende da **interface** Repository, não da impl.
**Slices só se comunicam por evento** (nunca importam entity/repo um do outro; podem importar o contrato de evento).

**Handlers auto-registráveis (o slice é dono das suas rotas):** cada slice expõe no `handler.go` um
`func (h *Handler) Register(r fiber.Router)` (ou `RegisterPublic`/`RegisterV1` quando precisa distinguir rota
pública de autenticada) que monta **todas** as rotas daquele domínio. O `cmd/api` só **compõe** —
`identity.Register(v1)`, `processo.Register(v1)`, … — e **não conhece nenhuma rota individual**. Adicionar um
domínio = criar o slice + **uma linha** de `Register` na composição da API. A `main` nunca lista `app.Get/Post`.

## Stack / libs (usar a versão instalada; conferir doc via context7 antes de codar)
Fiber · sqlc + pgx/v5 · asynq + Redis · PostgreSQL + pgvector · golang-migrate · **ozzo-validation** (método
`Validate()` na Request, não tags) · **caarlos0/env/v11** (config) · **Clerk** SDK Go + **svix** (webhook) ·
OpenTelemetry-Go + **otelpgx** · slog · aws-sdk-go-v2 (S3/R2, presigned) · google/uuid (v7) · pgvector-go.

## Convenções que carregam peso
- **Identificadores em inglês** (tabelas/colunas/tipos), conforme `docs/erd-modelo-de-dados.md`. Enums = `text` + CHECK/validação na app.
- **tenant_id em toda tabela de usuário** + isolamento em 2 barreiras: filtro na app (`tenantID` em toda assinatura de repo, vindo do token, nunca do body) **e** RLS (`SET LOCAL app.tenant_id` por tx).
- **Erros tipados e agnósticos de HTTP**: `AppError`+`Kind` vivem em **`lib/apperr`** (livre de Fiber — domínio e repos importam sem puxar HTTP). O mapa `statusByKind` + `WriteError` ficam em `lib/httpx` (a borda, que conhece Fiber). 5xx loga `cause` com `trace_id`, não vaza ao cliente. "Não existe" = erro tipado `ENTITY_NOT_FOUND`, nunca `nil,nil`.
- **Escrita sempre em tx**: o **caso de uso** abre a tx (Unit of Work em `lib/database`); o repo *participa* dela. Entidade + `outbox` commitam juntos (transactional outbox).
- **Repo devolve a entidade mapeada** (`*Draft`), nunca a row do sqlc — o `mapper.go` absorve `pgtype.*`. Leitura de tela usa read model (DTO por query dedicada), não monta o agregado.
- **Eventos**: produtor grava no `outbox` na mesma tx; `worker-outbox-relay` publica no asynq (`FOR UPDATE SKIP LOCKED`); consumidor (`listener.go`) checa `processed_event` (idempotência, at-least-once). `trace_context` viaja no payload (trace ponta a ponta).
- **API**: prefixo `/v1`; paginação por **cursor** (envelope `{data, page:{next_cursor,limit}}`); upload por **presigned URL**; formato único de erro `{kind,message,details}`.
- **Auth (Clerk)**: `Clerk Org = tenant`, `1 user = 1 escritório`. Middleware verifica JWT (JWKS), resolve `org_id→tenant_id` interno em `principal{UserID,TenantID,Role}` no ctx; handler lê `tenant_id` já resolvido, nunca `org_id`. Provisionamento via webhook Clerk (assinatura svix verificada sempre).
- **Boot** (todo binário): config (`caarlos0/env`, `required` falha rápido) → health check das deps → migrations (**só o api**) → pools/asynq/otel → graceful shutdown (SIGTERM/SIGINT, drain 30s). Não sobe pela metade.
- **Config/segredos** só por env var; nunca no código nem commitados.

## Green gate (obrigatório em todo slice)
`go build ./... && go test ./...` deve sair 0. Testes: unitário (entidade + caso de uso com repo mockado) é o grosso;
integração (repo contra Postgres real, outbox→relay→listener) via docker compose; e2e poucos.
TDD para feature/bugfix: escreve o teste do critério de aceite primeiro.

## Ambiente local (docker compose) — como subir a stack pra testar

Três overlays compostos (cada um soma serviços ao anterior — sempre `-f docker-compose.yml -f ...`):

| Comando           | Sobe                                                          | Quando usar |
|--------------------|----------------------------------------------------------------|-------------|
| `make up`          | postgres + redis + api + 4 workers + scheduler                | testar só o BE (o padrão) |
| `make up-full`      | tudo de `up` + `web` (FE, repo irmão `../jus-assessoria-automatizada-fe`, `network_mode: host`, porta 3000) | testar FE+BE juntos — requer os dois repos clonados como irmãos |
| `make up-tunnel`    | tudo de `up-full` + `tunnel` (ngrok, porta 4040)               | testar signup/onboarding (Clerk precisa alcançar o webhook) |

`make down` / `down-full` / `down-tunnel` derruba cada nível. Nenhum destes overlays roda em CI — são só pra dev local
(ver `docker-compose.fe.yml`, `docker-compose.tunnel.yml`, comentários nos arquivos têm o porquê de cada decisão).

**Pré-requisito**: `.env` na raiz (copiar de `.env.example`) com `DATABASE_URL`/`REDIS_URL` (irrelevantes pro compose —
ele sobrescreve pra apontar pros containers — mas exigidos pelo `config.Load`), `CLERK_SECRET_KEY`/`CLERK_ISSUER`,
`ANTHROPIC_API_KEY` (só precisa existir, não precisa ser válida se não for testar IA de verdade). Pra `up-full`:
+ `NEXT_PUBLIC_CLERK_PUBLISHABLE_KEY`. Pra `up-tunnel`: + `NGROK_AUTHTOKEN` (free tier — pegar em
https://dashboard.ngrok.com/get-started/your-authtoken, **não** confundir com a API Key da mesma conta, que
autentica na REST API do ngrok mas o agente do túnel rejeita com `ERR_NGROK_107`).

**Testar signup + onboarding de ponta a ponta** (a organização/tenant nasce via `createOrganization` no Clerk client-
side → Clerk dispara webhook `organization.created` → `POST /webhooks/clerk` (rota pública, raiz do app, **não** é
`/v1/...` — ver `internal/identity/webhook.go:48`) → BE provisiona o tenant → FE poll em `/v1/identity/me` até achar):
1. `make up-tunnel`, depois `curl -s http://localhost:4040/api/tunnels | jq -r '.tunnels[0].public_url'` pra pegar a URL pública.
2. Cadastrar essa URL + `/webhooks/clerk` como Endpoint em https://dashboard.clerk.com (Webhooks), evento
   `organization.created` (e `organizationMembership.created` se for testar convite de time). Isso gera um
   `CLERK_WEBHOOK_SECRET` novo — colar em `.env` e `docker compose ... up -d api` pra recarregar.
3. A URL do túnel é efêmera por padrão (muda a cada restart do container `tunnel`) — domínio fixo é plano pago do
   ngrok agora (não é mais free tier). Não reiniciar o container `tunnel` no meio de uma sessão de teste.
4. Signup: usar e-mail no formato `algo+clerk_test@example.com` (Clerk aceita sem enviar e-mail de verdade) e o
   código OTP fixo `424242` — funciona porque a chave é `sk_test_...` (instância de dev, sem Turnstile/captcha).

## Fluxo de git e tags (obrigatório — vários devs em slices separados)
Nunca commitar direto na `main` (é prod). Sempre: `git checkout main && git pull` → branch nova a partir dela →
commits na branch → merge de volta na `main`, resolvendo conflito quando aparecer (comum com vários devs mexendo em
slices ao mesmo tempo).

Tags SemVer (`vMAJOR.MINOR.PATCH`) criadas **após** o merge na `main`, nunca antes:
- Primeiro merge do v0 em produção → `v1.0.0`.
- Feature nova (endpoint, slice, fatia vertical) → bump de MINOR (`v1.1.0`, `v1.2.0`, ...).
- Bugfix/hotfix → bump de PATCH (`v1.0.1`, `v1.0.2`, ...).
- Breaking change (contrato de API, schema incompatível) → bump de MAJOR.

## Skills do projeto (INVOCAR com a Skill tool antes de codar — não codar de memória)
`golang-code-style` · `golang-design-patterns` · `golang-error-handling` · `golang-testing`.
Rote pelo que a mudança toca; invoque só os que se aplicam. Frontend (quando vier): `docs/erd-frontend.md` (ainda não recebido).
