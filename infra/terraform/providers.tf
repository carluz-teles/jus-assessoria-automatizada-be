# providers.tf — provider Railway + backend do state remoto (docs/erd-backend.md §5e.2, §5e.5).
#
# Toda a infra do Railway (projeto, ambientes, serviços, banco, Redis, variáveis,
# domínio) é gerenciada pelo provider community `terraform-community-providers/railway`.
# O bucket S3-compatível dos PDFs/payloads (storage.tf) vive FORA do Railway; é
# provisionado pelo provider `aws` apontado para um endpoint S3-compatível (S3/R2/MinIO).
#
# State REMOTO, nunca local (§5e.5): fica num bucket S3 versionado com lock. O green
# gate valida com `-backend=false`, então o backend é declarado mas não inicializado
# durante a validação; `terraform init` real (CI) recebe as credenciais do backend.

terraform {
  required_version = "~> 1.9"

  required_providers {
    railway = {
      source  = "terraform-community-providers/railway"
      version = "~> 0.5"
    }
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }

  backend "s3" {
    bucket = "court-legal-tfstate"
    key    = "railway/terraform.tfstate"
    # region/encrypt/lock são passados no `terraform init` real (CI/vault). O lock
    # usa DynamoDB (< 1.10) ou `use_lockfile` (>= 1.10) conforme o runtime do CI.
  }
}

provider "railway" {
  token = var.railway_token # do ambiente/vault, NUNCA commitado (§5e.5)
}
