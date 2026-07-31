#!/usr/bin/env bash
# infra/terraform/import.sh — ADOÇÃO: traz o court-legal existente pro state do Terraform.
#
# Rode UMA vez, antes do 1º apply.sh. Idempotente (só importa o que falta no state).
# Importa o PROJETO + os 8 SERVIÇOS. NÃO importa as variable collections — o apply.sh as
# cria via upsert (reconcilia e adiciona as vars de billing/notifications). O volume vem
# junto do serviço postgres (bloco nested).
#
# SEGURO: `terraform import` só ESCREVE O STATE — não altera a infra na Railway.
set -euo pipefail
cd "$(dirname "$0")"

# shellcheck source=_env.sh
source ./_env.sh

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

# Project id do court-legal (estável). Resolve os serviços a partir dele.
PID="${COURT_LEGAL_PROJECT_ID:-0f0790a9-235b-499d-af63-c8f83b5dba0b}"

log "terraform init…"
terraform init -input=false -no-color

log "Resolvendo serviços do court-legal ($PID)…"
MAP="$(RAILWAY_TOKEN="$RAILWAY_TOKEN" PID="$PID" python3 - <<'PY'
import os,json,urllib.request
b=json.dumps({"query":"query($id:String!){ project(id:$id){ services{edges{node{id name}}} } }","variables":{"id":os.environ["PID"]}}).encode()
r=urllib.request.Request("https://backboard.railway.app/graphql/v2",data=b,headers={"Authorization":"Bearer "+os.environ["RAILWAY_TOKEN"],"Content-Type":"application/json","User-Agent":"curl/8.5.0"})
p=json.load(urllib.request.urlopen(r,timeout=30))["data"]["project"]
for e in p["services"]["edges"]:
    n=e["node"]["name"]; sid=e["node"]["id"]
    addr = f'railway_service.{n}' if n in ("postgres","redis") else f'railway_service.app["{n}"]'
    print(f"{addr}\t{sid}")
PY
)"
[ -n "$MAP" ] || { log "Nenhum serviço encontrado — projeto/token certo?"; exit 1; }

imp() { # <addr> <id>
  local addr="$1" id="$2"
  if terraform state list 2>/dev/null | grep -qxF "$addr"; then
    printf '  = já no state: %s\n' "$addr"
  else
    log "import $addr"
    terraform import -input=false -no-color "$addr" "$id"
  fi
}

imp "railway_project.court_legal" "$PID"
while IFS=$'\t' read -r addr id; do
  [ -n "$addr" ] || continue
  imp "$addr" "$id"
done <<< "$MAP"

log "Import concluído. Rode: terraform plan (deve mostrar só as collections a criar + as"
log "vars novas de billing/notifications). Só então ./apply.sh."
