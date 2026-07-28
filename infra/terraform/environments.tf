# environments.tf — ambientes staging e prod, do mesmo código (docs §5e.1).
#
# Ambos são declarados; var.environment escolhe para qual as variáveis e o domínio
# são aplicados neste run (local.active_environment_id). Assim staging e prod saem
# do mesmo módulo, parametrizados por tfvars — some o "funciona em staging, não em prod".
#
# NOTA de apply: o Railway cria um environment default junto com o projeto. Na
# primeira aplicação, importe-o (`terraform import railway_environment.prod <id>`)
# ou renomeie-o, para o Terraform não tentar criar um duplicado. Detalhe no README.

resource "railway_environment" "prod" {
  name       = "production"
  project_id = railway_project.main.id
}

resource "railway_environment" "staging" {
  name       = "staging"
  project_id = railway_project.main.id
}

locals {
  active_environment_id = var.environment == "prod" ? railway_environment.prod.id : railway_environment.staging.id
}
