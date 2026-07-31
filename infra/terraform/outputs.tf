# outputs.tf — o que inspecionar depois do apply.
output "project_id" {
  description = "ID do projeto court-legal."
  value       = railway_project.court_legal.id
}

# O FE (repo separado) consome este id como STG_ENVIRONMENT_ID / env de deploy.
output "environment_id" {
  description = "ID do environment production do court-legal (onde o web do FE também deploya)."
  value       = railway_project.court_legal.default_environment.id
}

output "app_service_ids" {
  description = "Mapa nome->id dos serviços de app."
  value       = { for k, s in railway_service.app : k => s.id }
}

output "image_tag_deployed" {
  description = "Tag de imagem fixada nos serviços de app (github.sha em CI)."
  value       = var.image_tag
}
