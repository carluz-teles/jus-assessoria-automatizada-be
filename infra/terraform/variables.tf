# variables.tf — inputs do módulo (docs/erd-backend.md §5e.3).
#
# Dois grupos:
#   1. Config de forma da infra (registry, tag, região, réplicas, domínio) — tem
#      default sensato e mora nos tfvars por ambiente (environments/*.tfvars.example).
#   2. SEGREDOS — sensíveis, SEM default, vindos do cofre/CI em tempo de apply
#      (§5e.5). NUNCA commitados em .tf nem em .tfvars.

# ── Forma da infra ───────────────────────────────────────────────────────────

variable "railway_token" {
  description = "Token da API do Railway. Vem do cofre/CI, nunca commitado."
  type        = string
  sensitive   = true
}

variable "project_name" {
  description = "Nome do projeto no Railway."
  type        = string
  default     = "court-legal"
}

variable "environment" {
  description = "Ambiente-alvo deste apply: 'prod' ou 'staging'. Seleciona para qual railway_environment as variáveis e o domínio são aplicados."
  type        = string
  default     = "prod"

  validation {
    condition     = contains(["prod", "staging"], var.environment)
    error_message = "environment deve ser 'prod' ou 'staging'."
  }
}

variable "image_registry" {
  description = "Registry + namespace das imagens (ex.: ghcr.io/jusassessoria). A imagem de cada serviço é <registry>/jus-<svc>:<tag>."
  type        = string
  default     = "ghcr.io/jusassessoria"
}

variable "image_tag" {
  description = "Tag da imagem a rodar. O pipeline de deploy builda :sha e reaplica; o Terraform só recebe a tag (§5e.4)."
  type        = string
  default     = "latest"
}

variable "region" {
  description = "Região Railway onde os serviços rodam."
  type        = string
  default     = "us-east4-eqdc4a"
}

variable "service_replicas" {
  description = "Réplicas por serviço, ajustável por ambiente (staging menor). A api recebe >= 1; workers conforme carga."
  type        = map(number)
  default = {
    api                 = 2
    worker-ingestao     = 1
    worker-documents    = 1
    worker-ai           = 1
    worker-outbox-relay = 1
    scheduler           = 1
  }
}

variable "api_domain" {
  description = "Domínio custom da api (railway_custom_domain). O output api_dns_record diz o CNAME a apontar."
  type        = string
  default     = "api.court-legal.app"
}

variable "postgres_user" {
  description = "Usuário do Postgres (não-secreto; a senha é sensível à parte)."
  type        = string
  default     = "jus"
}

variable "postgres_db" {
  description = "Database do Postgres."
  type        = string
  default     = "jus"
}

# ── Segredos (sensíveis, SEM default — vêm do cofre/CI no apply) ──────────────

variable "clerk_secret_key" {
  description = "CLERK_SECRET_KEY — SDK do Clerk (backend)."
  type        = string
  sensitive   = true
}

variable "clerk_webhook_secret" {
  description = "CLERK_WEBHOOK_SECRET — verificação da assinatura svix do webhook."
  type        = string
  sensitive   = true
}

variable "clerk_issuer" {
  description = "CLERK_ISSUER — issuer esperado no JWT."
  type        = string
  sensitive   = true
}

variable "anthropic_key" {
  description = "ANTHROPIC_API_KEY — chamadas ao modelo (worker-ai)."
  type        = string
  sensitive   = true
}

variable "otel_endpoint" {
  description = "OTEL_EXPORTER_OTLP_ENDPOINT — destino OTLP (New Relic; trocável por env, §I8)."
  type        = string
  sensitive   = true
}

variable "otel_headers" {
  description = "OTEL_EXPORTER_OTLP_HEADERS — headers OTLP no formato key=value,key2=value2 (New Relic: api-key=<license>). Vazio = sem headers."
  type        = string
  sensitive   = true
  default     = ""
}

variable "postgres_password" {
  description = "Senha do Postgres. Compõe a DATABASE_URL por referência (variables_env.tf)."
  type        = string
  sensitive   = true
}

variable "s3_endpoint" {
  description = "S3_ENDPOINT — endpoint S3-compatível do R2: https://<account_id>.r2.cloudflarestorage.com."
  type        = string
  sensitive   = true
}

variable "s3_region" {
  description = "S3_REGION — região do bucket R2; para R2 é sempre \"auto\"."
  type        = string
  sensitive   = true
}

variable "s3_bucket" {
  description = "S3_BUCKET — nome do bucket R2 de PDFs/payloads brutos (criado à mão no painel R2)."
  type        = string
  sensitive   = true
}

variable "s3_access_key" {
  description = "S3_ACCESS_KEY — access key do token de API do R2."
  type        = string
  sensitive   = true
}

variable "s3_secret_key" {
  description = "S3_SECRET_KEY — secret key do token de API do R2."
  type        = string
  sensitive   = true
}
