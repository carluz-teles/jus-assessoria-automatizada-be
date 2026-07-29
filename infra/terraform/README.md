# infra/terraform — Railway via IaC

Provisiona **toda** a infra da plataforma no Railway por Terraform: projeto, ambientes
(prod/staging), os 6 serviços de app, Postgres (pgvector) + Redis, as env vars de cada
serviço e o domínio custom da api. O storage dos documentos é um bucket **Cloudflare R2**
criado **à mão** no painel do R2 (free tier); o Terraform não gerencia o bucket — só injeta
as env vars `S3_*` que o cliente de presigned URL da app consome. **Não há AWS.**

Fonte de verdade: `docs/erd-backend.md` §5e. **Nada do Railway é criado clicando no painel** —
o painel do Railway é read-only na prática.

## Adaptação à realidade do provider

O provider community `terraform-community-providers/railway` (v0.6.x) — confirmado com
`terraform providers schema -json` — **não tem** atributo de start/command. Não dá para
apontar todos os serviços à mesma imagem com comandos diferentes (como o esboço do §5e.3
imaginava). Em vez disso:

- **Seis imagens por-serviço**, `<registry>/jus-<svc>:<tag>`, cada uma com o ENTRYPOINT
  certo (o Dockerfile único builda um binário por `cmd/`, empacotado como `jus-<svc>`).
- `regions` e `volume` são **atributos aninhados** (sintaxe `= [{…}]` / `= {…}`), não blocos.
- `volume.size` é **computed** (definido pelo plano do Railway) — não é setável no HCL.

## Estrutura

```
providers.tf     provider railway + backend `cloud {}` (state no Terraform Cloud)
variables.tf     inputs: forma da infra (defaults) + segredos (sensíveis, sem default)
project.tf       railway_project
environments.tf  railway_environment prod + staging; var.environment escolhe o ativo
services.tf      os 6 serviços (for_each), cada um na sua imagem jus-<svc>
datastores.tf    postgres (pgvector + volume) e redis + suas variáveis
variables_env.tf env vars de cada serviço de app (DATABASE_URL/REDIS_URL + S3_* por referência)
domains.tf       domínio custom da api + outputs (DNS, project_id)
storage.tf       só doc: o bucket R2 é criado à mão; o TF só injeta as env vars S3_*
environments/
  prod.tfvars.example     tamanhos/réplicas de produção (SEM segredos)
  staging.tfvars.example  tamanhos menores, réplicas = 1 (SEM segredos)
```

## Storage (Cloudflare R2, criado à mão)

O bucket dos PDFs/payloads é um bucket **R2** (S3-compatível, free tier), criado **uma vez**
no painel do Cloudflare R2 — o Terraform **não** o gerencia (não há provider AWS). O TF só
injeta as credenciais nos serviços via `variables_env.tf`. A config chega pelas variáveis
sensíveis `S3_*` (cofre/CI):

- `S3_ENDPOINT` = `https://<account_id>.r2.cloudflarestorage.com`
- `S3_REGION`   = `auto`
- `S3_BUCKET` / `S3_ACCESS_KEY` / `S3_SECRET_KEY` = bucket + token de API do R2.

## Segredos e state (não-negociáveis, §5e.5)

- **State remoto no Terraform Cloud, nunca local.** O bloco `cloud {}` é vazio: a organização
  e o workspace vêm do ambiente (`TF_CLOUD_ORGANIZATION` / `TF_WORKSPACE`), nunca hardcoded. O
  `init` real autentica no TF Cloud via `TF_TOKEN_app_terraform_io` (= `TF_API_TOKEN`). O green
  gate valida com `init -backend=false` (não contata o TF Cloud, provisiona nada).
- **Segredos fora do Terraform em claro.** `railway_token`, `clerk_*`, `anthropic_key`,
  `otel_endpoint`, `postgres_password`, `s3_*` (credenciais R2) são variáveis `sensitive`
  **sem default** — chegam do cofre/CI no apply (`TF_VAR_*` ou `-var`), nunca em `.tf` nem em
  `.tfvars` commitado. Os `*.tfvars.example` carregam só tamanho/réplica/região/imagem/domínio.

### GitHub Actions — secrets e variables necessários

- **Secret** `TF_API_TOKEN` — token de API do Terraform Cloud (mapeado para `TF_TOKEN_app_terraform_io`).
- **Variables** (repo, não-secretas) `TF_CLOUD_ORGANIZATION` e `TF_WORKSPACE` — org/workspace do state.
- **Secrets** `RAILWAY_TOKEN`, `CLERK_*`, `ANTHROPIC_API_KEY`, `OTEL_ENDPOINT`, `POSTGRES_PASSWORD`,
  `S3_ENDPOINT`/`S3_REGION`/`S3_BUCKET`/`S3_ACCESS_KEY`/`S3_SECRET_KEY` (R2) → `TF_VAR_*`.
- (Removidos nesta fatia: `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_REGION` — não há mais AWS.)

## Rodando

```bash
# 1. init real contra o Terraform Cloud (CI/local com TF_API_TOKEN)
export TF_TOKEN_app_terraform_io=…   # token de API do TF Cloud
export TF_CLOUD_ORGANIZATION=…  TF_WORKSPACE=…
terraform -chdir=infra/terraform init -input=false

# 2. plan do ambiente-alvo, segredos via TF_VAR_* (do cofre/CI)
export TF_VAR_railway_token=…  TF_VAR_postgres_password=…  TF_VAR_anthropic_key=…  # etc.
export TF_VAR_s3_endpoint=…  TF_VAR_s3_region=auto  TF_VAR_s3_bucket=…  # credenciais R2
terraform -chdir=infra/terraform plan \
  -var-file=environments/prod.tfvars \
  -out=tfplan

# 3. apply do artefato revisado — DECISÃO HUMANA/CI, nunca automática
terraform -chdir=infra/terraform apply tfplan
```

**Apply é decisão humana/CI**, sobre um plano revisado — nunca `-auto-approve`, nunca direto
em produção sem revisão (§5e.4/§5e.6). Terraform muda quando a **forma** da infra muda; o
deploy de código (build + push da imagem :sha) é outro fluxo e não toca no Terraform.

### Green gate (validação, sem tocar em nuvem)

```bash
terraform -chdir=infra/terraform fmt -recursive
terraform -chdir=infra/terraform init -backend=false
terraform -chdir=infra/terraform validate
```

`init -backend=false` valida offline com o `cloud {}` vazio, sem contatar o TF Cloud. Não rode
`plan`/`apply` contra o Railway/TF Cloud sem credenciais — provisionaria nuvem real.

### Primeiro apply — environment default

O Railway cria um environment default junto com o projeto. Antes do primeiro apply, importe-o
para não duplicar `production`:

```bash
terraform -chdir=infra/terraform import railway_environment.prod <default-env-id>
```

## Notas de infra pendentes

- **I8 — OTLP → New Relic:** a telemetria padroniza em OTEL; o destino é config. O endpoint OTLP
  entra por `OTEL_EXPORTER_OTLP_ENDPOINT` (var sensível `otel_endpoint`), apontando para o New
  Relic. Trocar de backend OTLP não muda uma linha de instrumentação (docs §6, §5e.7).
- **I9 — Backup do Postgres (pg_dump):** o volume do Postgres é persistente, mas backup é à parte.
  Configurar um job de `pg_dump` periódico (Railway cron ou pipeline externo) despejando para o
  bucket R2 do `storage.tf`, com retenção. Ainda não modelado como recurso Terraform — candidato a
  `railway_service` com `cron_schedule` numa próxima fatia.
```
