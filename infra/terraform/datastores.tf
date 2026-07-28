# datastores.tf — Postgres (pgvector) e Redis como railway_service (docs §5e.3).
#
# Postgres roda a imagem pgvector/pgvector:pg16 (o schema usa pgvector, docs
# erd-modelo-de-dados). Volume persistente montado no datadir. `volume.size` é
# COMPUTED no provider (definido pelo plano do Railway), então NÃO é setável aqui —
# só name e mount_path. Redis é o redis:7-alpine, efêmero (fila asynq).

resource "railway_service" "postgres" {
  name         = "postgres"
  project_id   = railway_project.main.id
  source_image = "pgvector/pgvector:pg16"

  volume = {
    name       = "postgres-data"
    mount_path = "/var/lib/postgresql/data"
  }
}

resource "railway_service" "redis" {
  name         = "redis"
  project_id   = railway_project.main.id
  source_image = "redis:7-alpine"
}

# Credenciais/DB do Postgres. A senha é sensível; user/db são config. O for_each
# itera as CHAVES (não-sensíveis) e busca o valor à parte — for_each não aceita
# coleção sensível.
locals {
  postgres_vars = {
    POSTGRES_USER     = var.postgres_user
    POSTGRES_PASSWORD = var.postgres_password
    POSTGRES_DB       = var.postgres_db
  }
}

resource "railway_variable" "postgres" {
  for_each = toset(keys(local.postgres_vars))

  name           = each.key
  value          = local.postgres_vars[each.key]
  environment_id = local.active_environment_id
  service_id     = railway_service.postgres.id
}
