# services.tf — os 6 serviços de aplicação (docs §5e.3).
#
# ADAPTAÇÃO À REALIDADE DO PROVIDER: o `railway_service` do provider community
# NÃO tem atributo de start/command/run. Então não dá para apontar todos os
# serviços para a MESMA imagem com comandos diferentes (como o esboço do §5e.3
# imaginava). Em vez disso, rodamos SEIS imagens por-serviço — cada uma já traz o
# ENTRYPOINT certo (o Dockerfile único builda um binário por cmd/, empacotado como
# jus-<svc>). O serviço só aponta para a SUA imagem: <registry>/jus-<svc>:<tag>.
#
# `regions` é um ATRIBUTO aninhado (nested_type list), não um bloco — daí a sintaxe
# `regions = [{ ... }]`. Confirmado por `terraform providers schema -json`.

locals {
  # Os nomes batem com os binários de cmd/ e com a imagem jus-<svc> do slice Docker.
  app_services = [
    "api",
    "worker-ingestao",
    "worker-documents",
    "worker-ai",
    "worker-outbox-relay",
    "scheduler",
  ]
}

resource "railway_service" "app" {
  for_each = toset(local.app_services)

  name         = each.key
  project_id   = railway_project.main.id
  source_image = "${var.image_registry}/jus-${each.key}:${var.image_tag}"

  regions = [{
    region       = var.region
    num_replicas = lookup(var.service_replicas, each.key, 1)
  }]
}
