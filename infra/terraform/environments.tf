# environments.tf — ambiente de produção (docs §5e.1). Prod-only nesta fase.
#
# O Railway cria um environment "production" DEFAULT junto com o projeto. Criar um
# recurso com esse mesmo nome colide ("environment with that name already exists").
# Por isso o módulo gerencia um environment PRÓPRIO chamado "prod" (nome distinto do
# default) — assim o apply funciona from-scratch, sem hardcode de ID nem import, e o
# default "production" fica ocioso. Todas as variáveis de app, os datastores e o
# domínio penduram neste environment via local.active_environment_id.

resource "railway_environment" "prod" {
  name       = "prod"
  project_id = railway_project.main.id
}

locals {
  active_environment_id = railway_environment.prod.id
}
