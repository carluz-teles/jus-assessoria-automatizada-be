#!/usr/bin/env bash
# infra/terraform/apply.sh — apply CONVERGENTE e serial do court-legal.
#
# Converge-loop (NÃO "apply atômico one-shot"): o backend da Railway processa
# criação/variableCollectionUpsert de forma ASSÍNCRONA e lossy — o apply pode ter FALSO
# sucesso (env some, serviço some) mesmo dizendo "N added". Mas o `plan` DETECTA o drift e
# um 2º apply CONVERGE. Então: apply -> plan; se ainda houver diff, re-apply; até `plan`
# limpar (teto MAX_ATTEMPTS). SEM destroy-on-failure — num update de infra existente,
# destruir por não-convergência seria catastrófico; se não convergir, ALERTA e sai != 0.
#
# Projeto court-legal-stg é CRIADO do zero (sem import). THROTTLE: -parallelism=1.
# Envs: via _env.sh (mesmos secrets do GitHub Actions).
set -euo pipefail
cd "$(dirname "$0")"

MAX_ATTEMPTS="${MAX_ATTEMPTS:-4}"
# shellcheck source=_env.sh
source ./_env.sh

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

log "terraform init…"
terraform init -input=false -no-color

# Converge-loop. plan -detailed-exitcode: 0 = sem mudanças, 2 = há diff, 1 = erro.
for attempt in $(seq 1 "$MAX_ATTEMPTS"); do
  log "apply (tentativa $attempt/$MAX_ATTEMPTS, parallelism=1)…"
  if ! terraform apply -input=false -no-color -auto-approve -parallelism=1; then
    log "ERRO no apply (tentativa $attempt). Saindo != 0 — corrija e re-rode (nada é destruído)."
    exit 1
  fi

  log "plan de verificação (o drift da Railway aterrissou?)…"
  set +e
  terraform plan -input=false -no-color -detailed-exitcode -parallelism=1 >/dev/null
  rc=$?
  set -e
  case "$rc" in
    0) log "CONVERGIU na tentativa $attempt — plan limpo, infra == config."
       terraform output -no-color || true
       exit 0 ;;
    2) log "Ainda há drift (a Railway perdeu algo) — re-aplicando…" ;;
    *) log "ERRO no plan de verificação (exit=$rc). Saindo != 0."
       exit 1 ;;
  esac
done

log "NÃO CONVERGIU após $MAX_ATTEMPTS tentativas. Nada destruído. Rode de novo ou investigue."
exit 1
