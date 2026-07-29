# providers.tf — provider Railway + backend do state remoto (docs/erd-backend.md §5e.2, §5e.5).
#
# Toda a infra do Railway (projeto, ambientes, serviços, banco, Redis, variáveis,
# domínio) é gerenciada pelo provider community `terraform-community-providers/railway`.
# O storage dos PDFs/payloads é um bucket Cloudflare R2 criado MANUALMENTE no painel
# (S3-compatível, fora do Terraform — ver storage.tf); o Terraform só injeta as env
# vars S3_* nos serviços (variables_env.tf). NÃO há provider AWS.
#
# State REMOTO no Terraform Cloud (§5e.5), nunca local. O bloco `cloud {}` é vazio: a
# organização e o workspace vêm do ambiente (TF_CLOUD_ORGANIZATION / TF_WORKSPACE),
# nunca hardcoded. O green gate valida com `terraform init -backend=false` (sem contatar
# o TF Cloud); o `init` real (CI/apply) autentica via TF_TOKEN_app_terraform_io.
#
# CI: merge em main tocando infra/terraform/** dispara plan+apply automático
# (.github/workflows/terraform.yml). O workspace TFC precisa estar em Execution
# Mode = Local para o apply enxergar os TF_VAR_* do runner do GitHub Actions.

terraform {
  required_version = "~> 1.9"

  required_providers {
    railway = {
      source  = "terraform-community-providers/railway"
      version = "~> 0.5"
    }
  }

  cloud {}
}

provider "railway" {
  token = var.railway_token # do ambiente/vault, NUNCA commitado (§5e.5)
}
