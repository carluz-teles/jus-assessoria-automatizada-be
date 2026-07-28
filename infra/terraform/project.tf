# project.tf — o projeto Railway (docs/erd-backend.md §5e.2).
# Tudo o mais (ambientes, serviços, banco, variáveis, domínio) pendura aqui.

resource "railway_project" "main" {
  name    = var.project_name
  private = true
}
