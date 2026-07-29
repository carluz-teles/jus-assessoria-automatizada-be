# environments.tf — ambiente de produção (docs §5e.1). Prod-only nesta fase.
#
# O Railway cria um environment "production" DEFAULT junto com o projeto. Criar um
# recurso railway_environment "production" colide ("environment with that name already
# exists"). Em vez disso, referenciamos o existente por ID (var.railway_prod_environment_id)
# — identificador, não segredo, mesmo padrão de railway_workspace_id. Todas as variáveis
# de app, os datastores e o domínio penduram neste environment via local.active_environment_id.

locals {
  active_environment_id = var.railway_prod_environment_id
}
