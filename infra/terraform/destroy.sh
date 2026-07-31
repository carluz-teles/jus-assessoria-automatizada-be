#!/usr/bin/env bash
# infra/terraform/destroy.sh — derruba a infra do court-legal (serial).
# ⚠️ PERIGO: isto apaga o projeto PRODUTIVO e o volume do Postgres (com dados). Só use
# num ambiente descartável (ex.: PROJECT_NAME=tf-sandbox). Em prod, NÃO rode.
set -euo pipefail
cd "$(dirname "$0")"

# shellcheck source=_env.sh
source ./_env.sh

terraform init -input=false -no-color
terraform destroy -input=false -no-color -auto-approve -parallelism=1
