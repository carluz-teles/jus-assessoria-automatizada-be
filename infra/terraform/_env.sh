#!/usr/bin/env bash
# infra/terraform/_env.sh — mapeamento ÚNICO env do projeto -> TF_VAR_* (DRY).
# Sourced por apply.sh, destroy.sh e import.sh. Não roda sozinho.
#
# TODA variável entra por env var — as MESMAS do GitHub Actions. RAILWAY_TOKEN o provider
# lê direto do env (não vira TF_VAR). ⚠️ Secret do Actions é write-only: só "puxa" DENTRO
# de um run do Actions; localmente exporte (set -a; source .env.railway; set +a).

# Obrigatórias (o provider e/ou o config.Load exigem):
: "${RAILWAY_TOKEN:?RAILWAY_TOKEN obrigatório (o provider railway lê deste env)}"
: "${RAILWAY_WORKSPACE_ID:?RAILWAY_WORKSPACE_ID obrigatório}"
: "${POSTGRES_PASSWORD:?}"
: "${CLERK_SECRET_KEY:?}"
: "${CLERK_WEBHOOK_SECRET:?}"
: "${S3_ENDPOINT:?}"; : "${S3_REGION:?}"; : "${S3_BUCKET:?}"; : "${S3_ACCESS_KEY:?}"; : "${S3_SECRET_KEY:?}"

# Backend de state (Terraform Cloud). Org vem por env (você define); workspace default do BE.
# Auth: exporte TF_TOKEN_app_terraform_io=<TFC API token> (ou `terraform login`).
# Org: Autojus (defina TF_CLOUD_ORGANIZATION=Autojus). Workspace do BE: autojus-terraform.
: "${TF_CLOUD_ORGANIZATION:?TF_CLOUD_ORGANIZATION obrigatório (org do Terraform Cloud = Autojus)}"
export TF_CLOUD_ORGANIZATION
export TF_WORKSPACE="${TF_WORKSPACE:-autojus-terraform}"

export TF_VAR_railway_workspace_id="$RAILWAY_WORKSPACE_ID"
export TF_VAR_project_name="${PROJECT_NAME:-court-legal-stg}"
export TF_VAR_image_registry="${IMAGE_REGISTRY:-ghcr.io/carluz-teles}"
export TF_VAR_image_tag="${IMAGE_TAG:-latest}" # em CI = github.sha (versionado)
export TF_VAR_postgres_user="${POSTGRES_USER:-jus}"
export TF_VAR_postgres_password="$POSTGRES_PASSWORD"
export TF_VAR_postgres_db="${POSTGRES_DB:-jus}"
export TF_VAR_clerk_secret_key="$CLERK_SECRET_KEY"
export TF_VAR_clerk_webhook_secret="$CLERK_WEBHOOK_SECRET"
export TF_VAR_clerk_issuer="${CLERK_ISSUER:-}"
export TF_VAR_anthropic_api_key="${ANTHROPIC_API_KEY:-}"
export TF_VAR_otel_exporter_otlp_endpoint="${OTEL_EXPORTER_OTLP_ENDPOINT:-}"
export TF_VAR_otel_exporter_otlp_headers="${OTEL_EXPORTER_OTLP_HEADERS:-}"
export TF_VAR_stripe_secret_key="${STRIPE_SECRET_KEY:-}"
export TF_VAR_stripe_webhook_secret="${STRIPE_WEBHOOK_SECRET:-}"
export TF_VAR_stripe_trial_days="${STRIPE_TRIAL_DAYS:-0}"
export TF_VAR_billing_success_url="${APP_BILLING_SUCCESS_URL:-}"
export TF_VAR_billing_cancel_url="${APP_BILLING_CANCEL_URL:-}"
export TF_VAR_billing_return_url="${APP_BILLING_RETURN_URL:-}"
export TF_VAR_resend_api_key="${RESEND_API_KEY:-}"
export TF_VAR_resend_from_email="${RESEND_FROM_EMAIL:-}"
export TF_VAR_resend_webhook_secret="${RESEND_WEBHOOK_SECRET:-}"
export TF_VAR_s3_endpoint="$S3_ENDPOINT"
export TF_VAR_s3_region="$S3_REGION"
export TF_VAR_s3_bucket="$S3_BUCKET"
export TF_VAR_s3_access_key="$S3_ACCESS_KEY"
export TF_VAR_s3_secret_key="$S3_SECRET_KEY"
