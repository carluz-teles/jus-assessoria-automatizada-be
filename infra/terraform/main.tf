# main.tf — infra do BE (court-legal) na Railway via Terraform, substituindo o provision.sh.
#
# ADOÇÃO SOBRE INFRA EXISTENTE: o court-legal e seus 8 serviços JÁ EXISTEM. Rode o import.sh
# UMA vez pra trazê-los pro state antes do 1º apply (senão o apply tentaria recriar tudo).
# As variable collections NÃO se importam — o apply as cria via upsert, reconciliando (e
# adicionando as vars de billing/notifications que o provision.sh não tinha).
#
# Decisões (travadas com o dono):
#  - THROTTLE: apply.sh usa `-parallelism=1` (serial). Ataca o erro de "criar muita
#    estrutura ao mesmo tempo".
#  - CONVERGÊNCIA (não "atômico one-shot"): o teste provou que o apply da Railway é
#    lossy/assíncrono (upserts de variável se perdem, serviço some) e reporta falso sucesso.
#    Então o apply.sh re-aplica até `plan` limpar. SEM destroy-on-failure (catastrófico num
#    update de infra existente).
#  - O serviço `web` do FE vive NESTE projeto mas é gerenciado pelo TF do repo FE (state
#    separado); este módulo NÃO o declara.
#  - VARIÁVEIS POR SERVIÇO: cada binário recebe SÓ o que consome. Como o config.Load()
#    é monolítico e exige 5 vars `required` em todo binário (DATABASE_URL, REDIS_URL,
#    CLERK_SECRET_KEY, ANTHROPIC_API_KEY, OTEL_EXPORTER_OTLP_ENDPOINT), essas formam a
#    base_vars compartilhada; os extras são por serviço (o que cada cmd/*/main.go lê).
#
# ALVO STG: criamos um environment "stg" (separado do "production" default) e penduramos
# nele as variable collections + o domínio auto-gerado. Produção fica intocada.

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
    # worker-ingestao: roda o listener de notifications (envia e-mail via Resend).
    "worker-ingestao" = merge(local.base_vars, {
      RESEND_API_KEY    = var.resend_api_key
      RESEND_FROM_EMAIL = var.resend_from_email
    })
    # Skeletons por ora — só a base (config.Load exige os 5 required).
    "worker-ai"           = local.base_vars
    "worker-documents"    = local.base_vars
    "worker-outbox-relay" = local.base_vars
    "scheduler"           = local.base_vars
  }

  # Variáveis do Postgres. PGDATA aponta p/ subdir do mount (o volume monta com
  # lost+found e o initdb recusa datadir não-vazio — mesma razão do provision.sh).
  postgres_vars = {
    POSTGRES_USER     = var.postgres_user
    POSTGRES_PASSWORD = var.postgres_password
    POSTGRES_DB       = var.postgres_db
    PGDATA            = "/var/lib/postgresql/data/pgdata"
  }

  # ALVO = ambiente STG. Os serviços são project-level (compartilhados), mas as variable
  # collections e o domínio penduram no environment "stg". A produção (env production,
  # gerenciada pelo provision.sh) fica INTOCADA — o TF só cria/gerencia o stg.
  environment_id = railway_environment.stg.id
}

# ===== Projeto (raiz; environment "production" default já existe) =====
# O court-legal JÁ EXISTE (criado pelo provision.sh) — este recurso é IMPORTADO pro state
# via import.sh, não recriado. Renomear/mudar workspace = recriar = perder tudo.
resource "railway_project" "court_legal" {
  name         = var.project_name
  description  = "jus-assessoria — BE + FE (serviço web do FE é gerenciado pelo TF do repo FE)."
  workspace_id = var.railway_workspace_id

  default_environment = {
    name = "production"
  }
}

# ===== Environment STG (novo; onde este TF deploya) =====
# Ambiente de staging isolado da produção. Domínio auto-gerado (sem domínio próprio ainda).
resource "railway_environment" "stg" {
  name       = "stg"
  project_id = railway_project.court_legal.id
}

# ===== Domínio auto-gerado da Railway pro api no stg (não é custom domain) =====
resource "railway_service_domain" "api" {
  subdomain      = var.api_subdomain
  environment_id = railway_environment.stg.id
  service_id     = railway_service.app["api"].id
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
