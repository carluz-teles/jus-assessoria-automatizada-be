# domains.tf — domínio custom da api (docs §5e.2).
#
# Aponta para o serviço api no ambiente ativo, na porta interna 8080 (Config.Port
# default). O DNS a apontar sai no output api_dns_record (CNAME → valor do provider).

resource "railway_custom_domain" "api" {
  domain         = var.api_domain
  environment_id = local.active_environment_id
  service_id     = railway_service.app["api"].id
  target_port    = 8080
}

output "api_dns_record" {
  description = "Registro DNS a criar no provedor de domínio para ativar o domínio custom da api."
  value = {
    name  = railway_custom_domain.api.domain
    type  = "CNAME"
    value = railway_custom_domain.api.dns_record_value
  }
}

output "project_id" {
  description = "Identificador do projeto Railway."
  value       = railway_project.main.id
}
