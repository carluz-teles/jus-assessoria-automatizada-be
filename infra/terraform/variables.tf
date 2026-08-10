# variables.tf — TODA variável entra por env var (TF_VAR_*), as MESMAS do GitHub
# Actions, exatamente como infra/railway/provision.sh. Nenhum default de segredo;
# nenhum valor commitado. O apply.sh mapeia os nomes usados no resto do projeto
# (CLERK_SECRET_KEY, ...) para os TF_VAR_* correspondentes.

# ---- Railway / projeto ----
variable "railway_workspace_id" {
  type        = string
  description = "Workspace (team) onde o projeto de teste será criado."
}

variable "project_name" {
  type        = string
  description = "Nome do projeto Railway (o court-legal de produção, já existente e importado)."
  default     = "court-legal"
}

variable "app_env" {
  type        = string
  description = "Valor de APP_ENV nos serviços."
  default     = "production"
}

# ---- Imagens (versionamento por SHA) ----
variable "image_registry" {
  type        = string
  description = "Registry das imagens de app (mesmo do provision.sh)."
  default     = "ghcr.io/carluz-teles"
}

variable "image_tag" {
  type        = string
  description = "Tag da imagem a fixar nos serviços de app. Em CI = github.sha (versionado, não ':latest')."
  default     = "latest"
}

# ---- Postgres ----
variable "postgres_user" {
  type    = string
  default = "jus"
}

variable "postgres_password" {
  type      = string
  sensitive = true
}

variable "postgres_db" {
  type    = string
  default = "jus"
}

# ---- Clerk ----
variable "clerk_secret_key" {
  type      = string
  sensitive = true
}

variable "clerk_webhook_secret" {
  type      = string
  sensitive = true
}

variable "clerk_issuer" {
  type    = string
  default = ""
}

# ---- IA / Observabilidade (opcionais, presentes mas podem ser vazios) ----
variable "anthropic_api_key" {
  type      = string
  sensitive = true
  default   = ""
}

variable "otel_exporter_otlp_endpoint" {
  type    = string
  default = ""
}

variable "otel_exporter_otlp_headers" {
  type      = string
  sensitive = true
  default   = ""
}

# ---- Stripe (billing) — só o serviço api consome ----
variable "stripe_secret_key" {
  type      = string
  sensitive = true
  default   = ""
}
variable "stripe_webhook_secret" {
  type      = string
  sensitive = true
  default   = ""
}
variable "stripe_trial_days" {
  type    = number
  default = 0
}
variable "billing_success_url" {
  type    = string
  default = "https://app.atjud.com.br/settings/billing?checkout=success"
}
variable "billing_cancel_url" {
  type    = string
  default = "https://app.atjud.com.br/settings/billing?checkout=canceled"
}
variable "billing_return_url" {
  type    = string
  default = "https://app.atjud.com.br/settings/billing?checkout=return"
}

# ---- Resend (notifications) — api usa o webhook secret; worker-ingestao envia e-mail ----
variable "resend_api_key" {
  type      = string
  sensitive = true
  default   = ""
}
variable "resend_from_email" {
  type    = string
  default = ""
}
variable "resend_webhook_secret" {
  type      = string
  sensitive = true
  default   = ""
}

# ---- Conector DJEN — proxy de saída (só o worker-ingestao consome) ----
# WAF do Comunica 403 o IP de datacenter da Railway; um proxy residencial/BR dá
# um IP de saída limpo. Sensível (traz credencial). Default vazio = conexão direta
# (no-op) — assim um apply sem o secret não quebra nada.
variable "djen_proxy_url" {
  type      = string
  sensitive = true
  default   = ""
}

# ---- Storage (R2/S3) ----
variable "s3_endpoint" {
  type = string
}
variable "s3_region" {
  type = string
}
variable "s3_bucket" {
  type = string
}
variable "s3_access_key" {
  type      = string
  sensitive = true
}
variable "s3_secret_key" {
  type      = string
  sensitive = true
}
