# versions.tf — pin do Terraform/provider + backend de state remoto (Terraform Cloud).
#
# Provider community da Railway (não há oficial), série 0.6.x. Saímos do TF antes na v0.6.2
# por bugs (volume "inconsistent result", region drift, rate limit, colisão de env, 404 no
# refresh); o teste de 2026-07-30 mostrou que com converge-loop + parallelism=1 ele é viável.
terraform {
  required_version = ">= 1.15.0" # o state no TFC foi escrito pelo 1.15.8; CLI mais velho não lê

  required_providers {
    railway = {
      source  = "terraform-community-providers/railway"
      version = "~> 0.6"
    }
  }

  # State remoto no Terraform Cloud (locking nativo — crucial pro converge-loop e pra evitar
  # corrida BE×FE no mesmo projeto). Org e workspace vêm por env (TF_CLOUD_ORGANIZATION e
  # TF_WORKSPACE), mantendo o padrão "tudo por env"; auth via TF_TOKEN_app_terraform_io.
  # BE usa o workspace court-legal-be; o FE (outro repo) usa court-legal-fe.
  cloud {}
}

# O provider lê o token do env var RAILWAY_TOKEN por padrão (mesmo token dos scripts
# e do GitHub Actions). Nada de segredo em arquivo.
provider "railway" {}
