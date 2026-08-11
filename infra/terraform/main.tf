# main.tf — infra do BE (court-legal, PRODUÇÃO) na Railway via Terraform, substituindo o
# provision.sh. (Staging fica pra depois — por ora movemos só com prod.)
#
# ADOÇÃO SOBRE INFRA EXISTENTE: o court-legal e seus 8 serviços JÁ EXISTEM. Rode o import.sh
# UMA vez (traz projeto+serviços pro state) antes do 1º apply — senão o apply tentaria recriar.
# As variable collections NÃO se importam: o apply as cria via upsert, reconciliando (e
# adicionando as vars de billing/notifications que o provision.sh não tinha). O domínio do
# api (auto-gerado pelo provision.sh) NÃO é gerenciado aqui — fica como está (follow-up).
#
# Decisões (travadas com o dono):
#  - THROTTLE: apply.sh usa `-parallelism=1` (serial). Ataca o erro de "criar muita
#    estrutura ao mesmo tempo".
#  - CONVERGÊNCIA (não "atômico one-shot"): o apply da Railway é lossy/assíncrono (upserts de
#    variável se perdem, serviço some) e reporta falso sucesso. Então o apply.sh re-aplica até
#    `plan` limpar. SEM destroy-on-failure (catastrófico num update de infra existente).
#  - O serviço `web` do FE vive NESTE projeto mas é gerenciado pelo TF do repo FE (state
#    separado); este módulo NÃO o declara.
#  - VARIÁVEIS POR SERVIÇO: cada binário recebe SÓ o que consome. Como o config.Load()
#    é monolítico e exige 5 vars `required` em todo binário (DATABASE_URL, REDIS_URL,
#    CLERK_SECRET_KEY, ANTHROPIC_API_KEY, OTEL_EXPORTER_OTLP_ENDPOINT), essas formam a
#    base_vars compartilhada; os extras são por serviço (o que cada cmd/*/main.go lê).

locals {
  # DATABASE_URL com a senha url-encoded (idêntico ao build_env_json do provision.sh).
  database_url = "postgres://${var.postgres_user}:${urlencode(var.postgres_password)}@postgres.railway.internal:5432/${var.postgres_db}?sslmode=disable"

  # Base OBRIGATÓRIA em todo serviço de app: os 5 `required` do config.Load() (senão o
  # binário nem boota) + os campos de telemetria/ambiente que todos usam.
  base_vars = {
    DATABASE_URL                = local.database_url
    REDIS_URL                   = "redis://redis.railway.internal:6379/0"
    CLERK_SECRET_KEY            = var.clerk_secret_key
    ANTHROPIC_API_KEY           = var.anthropic_api_key
    OTEL_EXPORTER_OTLP_ENDPOINT = var.otel_exporter_otlp_endpoint
    OTEL_EXPORTER_OTLP_HEADERS  = var.otel_exporter_otlp_headers
    APP_ENV                     = var.app_env
  }

  # Variáveis POR SERVIÇO = base + extras que aquele cmd/*/main.go realmente consome.
  # (Pruning total dos 5 `required` exigiria refatorar lib/config p/ ser por-binário —
  # fatia à parte; por ora a base fica em todos.)
  service_vars = {
    # api: HTTP server — Clerk (webhook/issuer), Stripe, Billing URLs, Resend webhook, S3, Port.
    "api" = merge(local.base_vars, {
      PORT                    = "8080"
      CLERK_ISSUER            = var.clerk_issuer
      CLERK_WEBHOOK_SECRET    = var.clerk_webhook_secret
      RESEND_WEBHOOK_SECRET   = var.resend_webhook_secret
      S3_ENDPOINT             = var.s3_endpoint
      S3_REGION               = var.s3_region
      S3_BUCKET               = var.s3_bucket
      S3_ACCESS_KEY           = var.s3_access_key
      S3_SECRET_KEY           = var.s3_secret_key
      STRIPE_SECRET_KEY       = var.stripe_secret_key
      STRIPE_WEBHOOK_SECRET   = var.stripe_webhook_secret
      STRIPE_TRIAL_DAYS       = tostring(var.stripe_trial_days)
      APP_BILLING_SUCCESS_URL = var.billing_success_url
      APP_BILLING_CANCEL_URL  = var.billing_cancel_url
      APP_BILLING_RETURN_URL  = var.billing_return_url
    })
    # worker-ingestao: roda o listener de notifications (envia e-mail via Resend) e
    # o backfill/sync do DJEN — que sai pelo proxy residencial (DJEN_PROXY_URL) p/
    # contornar o WAF que 403 o IP de datacenter.
    "worker-ingestao" = merge(local.base_vars, {
      RESEND_API_KEY    = var.resend_api_key
      RESEND_FROM_EMAIL = var.resend_from_email
      DJEN_PROXY_URL    = var.djen_proxy_url
      # DJEN pace/concorrência do sweet-spot (o teto do DJEN é ~2 req/s GLOBAL; acima
      # disso = 429). Com INGESTION_ENABLED o cutover troca o backfill per-OAB por
      # history-match contra o store (zero DJEN no worker) — a virada bulk.
      DJEN_RATE_PER_MINUTE = "120"
      INGESTAO_CONCURRENCY = "3"
      INGESTION_ENABLED    = "true"
    })
    # Skeletons por ora — só a base (config.Load exige os 5 required).
    "worker-ai"           = local.base_vars
    "worker-documents"    = local.base_vars
    "worker-outbox-relay" = local.base_vars
    # scheduler: re-poll + a INGESTÃO NACIONAL do diário DJEN (bulk). Sai pelo mesmo
    # proxy residencial; é o ÚNICO consumidor do orçamento DJEN quando o cutover está
    # ligado (o worker não bate mais no DJEN). BOOTSTRAP_DAYS=0 pula o cold-start
    # histórico; o loop diário ingere ontem + lookback.
    "scheduler" = merge(local.base_vars, {
      DJEN_PROXY_URL          = var.djen_proxy_url
      DJEN_RATE_PER_MINUTE    = "120"
      DJEN_PAGE_SIZE          = "1000"
      INGESTION_ENABLED       = "true"
      INGESTION_LOOKBACK_DAYS = "1"
      BOOTSTRAP_DAYS          = "0"
    })
  }

  # Variáveis do Postgres. PGDATA aponta p/ subdir do mount (o volume monta com
  # lost+found e o initdb recusa datadir não-vazio — mesma razão do provision.sh).
  postgres_vars = {
    POSTGRES_USER     = var.postgres_user
    POSTGRES_PASSWORD = var.postgres_password
    POSTGRES_DB       = var.postgres_db
    PGDATA            = "/var/lib/postgresql/data/pgdata"
  }

  # Environment production (default) do court-legal — onde os serviços rodam.
  environment_id = railway_project.court_legal.default_environment.id
}

# ===== Projeto court-legal (IMPORTADO — já existe, criado pelo provision.sh) =====
# Renomear/mudar workspace = recriar = perder tudo. Trazido pro state via import.sh.
resource "railway_project" "court_legal" {
  name         = var.project_name # court-legal
  description  = "jus-assessoria — BE + web do FE (FE gerenciado pelo TF do repo FE)."
  workspace_id = var.railway_workspace_id

  default_environment = {
    name = "production"
  }
}

# ===== Postgres COM volume =====
resource "railway_service" "postgres" {
  name         = "postgres"
  project_id   = railway_project.court_legal.id
  source_image = "pgvector/pgvector:pg16"

  # Nome CASA com o volume existente (postgres-volume) pro import bater sem diff.
  # Renomear = recriar = perda de dados.
  volume = {
    name       = "postgres-volume"
    mount_path = "/var/lib/postgresql/data"
  }
}

resource "railway_variable_collection" "postgres" {
  environment_id = local.environment_id
  service_id     = railway_service.postgres.id

  variables = [for k, v in local.postgres_vars : { name = k, value = v }]
}

# ===== Redis =====
resource "railway_service" "redis" {
  name         = "redis"
  project_id   = railway_project.court_legal.id
  source_image = "redis:7-alpine"
}

# ===== Serviços de app — imagem FIXADA por tag (versionamento, não ':latest') =====
resource "railway_service" "app" {
  for_each = local.service_vars

  name         = each.key
  project_id   = railway_project.court_legal.id
  source_image = "${var.image_registry}/jus-${each.key}:${var.image_tag}"
}

# ===== Uma variable collection POR SERVIÇO (cada um só o que consome) =====
resource "railway_variable_collection" "app" {
  for_each = local.service_vars

  environment_id = local.environment_id
  service_id     = railway_service.app[each.key].id

  variables = [for k, v in each.value : { name = k, value = v }]
}
