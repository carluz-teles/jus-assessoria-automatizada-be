# infra/terraform — Railway via IaC

Provisiona **toda** a infra da plataforma no Railway por Terraform: projeto, ambientes
(prod/staging), os 6 serviços de app, Postgres (pgvector) + Redis, as env vars de cada
serviço e o domínio custom da api. O bucket S3-compatível dos documentos é provisionado
ao lado, via provider `aws` apontado para o endpoint S3-compatível.

Fonte de verdade: `docs/erd-backend.md` §5e. **Nada é criado clicando no painel** — o
painel do Railway é read-only na prática.

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
providers.tf     provider railway + aws + backend s3 (state remoto)
variables.tf     inputs: forma da infra (defaults) + segredos (sensíveis, sem default)
project.tf       railway_project
environments.tf  railway_environment prod + staging; var.environment escolhe o ativo
services.tf      os 6 serviços (for_each), cada um na sua imagem jus-<svc>
datastores.tf    postgres (pgvector + volume) e redis + suas variáveis
variables_env.tf env vars de cada serviço de app (DATABASE_URL/REDIS_URL por referência)
domains.tf       domínio custom da api + outputs (DNS, project_id)
storage.tf       bucket S3-compatível (provider aws) + hardening
environments/
  prod.tfvars.example     tamanhos/réplicas de produção (SEM segredos)
  staging.tfvars.example  tamanhos menores, réplicas = 1 (SEM segredos)
```

## Segredos e state (não-negociáveis, §5e.5)

- **State remoto, nunca local.** Backend `s3` no bucket `court-legal-tfstate`, versionado e
  com lock. O green gate valida com `-backend=false` (backend declarado, não inicializado);
  o `init` real recebe as credenciais do backend.
- **Segredos fora do Terraform em claro.** `railway_token`, `clerk_*`, `anthropic_key`,
  `otel_endpoint`, `postgres_password`, `s3_*` são variáveis `sensitive` **sem default** —
  chegam do cofre/CI no apply (`TF_VAR_*` ou `-var`), nunca em `.tf` nem em `.tfvars`
  commitado. Os `*.tfvars.example` carregam só tamanho/réplica/região/imagem/domínio.

## Rodando

```bash
# 1. init com o backend s3 real (CI/local com credenciais do backend)
terraform -chdir=infra/terraform init \
  -backend-config="region=us-east-1" \
  -backend-config="encrypt=true"          # + lock (dynamodb_table < 1.10 ou use_lockfile >= 1.10)

# 2. plan do ambiente-alvo, segredos via TF_VAR_* (do cofre/CI)
export TF_VAR_railway_token=…  TF_VAR_postgres_password=…  TF_VAR_anthropic_key=…  # etc.
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
tflint --chdir=infra/terraform
```

Não rode `plan`/`apply` contra o Railway sem credenciais — provisionaria nuvem real.

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
  bucket S3-compatível de `storage.tf`, com retenção. Ainda não modelado como recurso Terraform —
  candidato a `railway_service` com `cron_schedule` numa próxima fatia.
