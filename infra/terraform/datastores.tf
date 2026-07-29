# datastores.tf — Postgres (pgvector) e Redis como railway_service (docs §5e.3).
#
# Postgres roda a imagem pgvector/pgvector:pg16 (o schema usa pgvector, docs
# erd-modelo-de-dados). Redis é o redis:7-alpine, efêmero (fila asynq).
#
# VOLUME fora do Terraform (mesmo padrão do bucket R2): o atributo `volume` inline do
# provider railway v0.6.2 produz "inconsistent result after apply" (bug do provider,
# sem fix na última versão) — taint→replace→colisão em loop. O volume persistente do
# Postgres (mount /var/lib/postgresql/data) é criado via API do Railway, uma vez,
# anexado a este serviço. Ver README (Notas de infra).

resource "railway_service" "postgres" {
  name         = "postgres"
  project_id   = railway_project.main.id
  source_image = "pgvector/pgvector:pg16"
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

resource "railway_variable_collection" "postgres" {
  environment_id = local.active_environment_id
  service_id     = railway_service.postgres.id

  variables = [for name, value in local.postgres_vars : { name = name, value = value }]
}
