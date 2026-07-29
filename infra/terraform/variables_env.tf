# variables_env.tf — env vars de cada serviço de app (docs §5e.3 + Config §5b.4).
#
# Injeta exatamente o que a struct Config (lib/config) lê. DATABASE_URL e REDIS_URL
# são CONSTRUÍDAS por REFERÊNCIA aos recursos postgres/redis (rede privada Railway,
# *.railway.internal) — não literais: recriar o banco atualiza a URL sozinho.
# Segredos vêm das variáveis sensíveis (cofre/CI), nunca hardcoded (§5e.5).

locals {
  database_url = "postgres://${var.postgres_user}:${urlencode(var.postgres_password)}@${railway_service.postgres.name}.railway.internal:5432/${var.postgres_db}?sslmode=disable"
  redis_url    = "redis://${railway_service.redis.name}.railway.internal:6379/0"

  # APP_ENV = "production" para Config.IsProduction() (prod-only nesta fase).
  app_env_name = "production"

  # As env vars de todo serviço de app. Workers ignoram PORT; a api ignora nada.
  app_env = {
    DATABASE_URL                = local.database_url
    REDIS_URL                   = local.redis_url
    CLERK_SECRET_KEY            = var.clerk_secret_key
    CLERK_WEBHOOK_SECRET        = var.clerk_webhook_secret
    CLERK_ISSUER                = var.clerk_issuer
    ANTHROPIC_API_KEY           = var.anthropic_key
    OTEL_EXPORTER_OTLP_ENDPOINT = var.otel_endpoint
    OTEL_EXPORTER_OTLP_HEADERS  = var.otel_headers
    APP_ENV                     = local.app_env_name
    PORT                        = "8080"
    S3_ENDPOINT                 = var.s3_endpoint
    S3_REGION                   = var.s3_region
    S3_BUCKET                   = var.s3_bucket
    S3_ACCESS_KEY               = var.s3_access_key
    S3_SECRET_KEY               = var.s3_secret_key
  }

  # Índice (svc → nome da var) NÃO-sensível: seguro para for_each. Os valores (que
  # carregam segredos) são buscados à parte, por chave, no atributo `value`.
  app_var_index = merge([
    for svc in local.app_services : {
      for name in keys(local.app_env) : "${svc}:${name}" => { service = svc, name = name }
    }
  ]...)
}

resource "railway_variable" "app" {
  for_each = local.app_var_index

  name           = each.value.name
  value          = local.app_env[each.value.name]
  environment_id = local.active_environment_id
  service_id     = railway_service.app[each.value.service].id
}
