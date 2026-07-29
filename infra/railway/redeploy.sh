#!/usr/bin/env bash
# infra/railway/redeploy.sh — dispara um redeploy dos serviços de app no Railway.
#
# O Railway NÃO faz pull automático quando uma tag do registry muda (não pollа o GHCR),
# então depois que o CI empurra as imagens (:latest + :sha) é preciso pedir o redeploy.
# Este script fecha o loop: acha o projeto pelo nome, pega o environment production e
# dispara serviceInstanceRedeploy em cada serviço de app (que puxa o :latest novo).
# Postgres/Redis usam imagens públicas e não mudam com o nosso build — ficam de fora.
#
# Env: RAILWAY_TOKEN (obrigatório), PROJECT_NAME (default court-legal).
set -euo pipefail

: "${RAILWAY_TOKEN:?RAILWAY_TOKEN obrigatório}"
export PROJECT_NAME="${PROJECT_NAME:-court-legal}"
export API="https://backboard.railway.app/graphql/v2"

# Serviços de app que rodam as nossas imagens jus-<svc>.
APP_SERVICES="api worker-ingestao worker-documents worker-ai worker-outbox-relay scheduler"

gql() {
  local __vars="${2:-}"; [ -n "$__vars" ] || __vars='{}'
  RAILWAY_QUERY="$1" RAILWAY_VARS="$__vars" python3 - <<'PY'
import os, json, urllib.request, urllib.error, sys
body = json.dumps({"query": os.environ["RAILWAY_QUERY"],
                   "variables": json.loads(os.environ["RAILWAY_VARS"])}).encode()
req = urllib.request.Request(os.environ["API"], data=body, headers={
    "Authorization": "Bearer " + os.environ["RAILWAY_TOKEN"],
    "Content-Type": "application/json",
    "User-Agent": "curl/8.5.0"})  # Cloudflare bloqueia o UA padrão do urllib (erro 1010)
try:
    r = json.load(urllib.request.urlopen(req, timeout=45))
except urllib.error.HTTPError as e:
    sys.stderr.write("HTTP %s: %s\n" % (e.code, e.read().decode()[:400])); sys.exit(1)
if r.get("errors"):
    sys.stderr.write("GQL ERROR: " + json.dumps(r["errors"])[:400] + "\n"); sys.exit(1)
print(json.dumps(r["data"]))
PY
}

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

log "Procurando projeto '$PROJECT_NAME'…"
read -r PROJECT_ID ENV_ID < <(gql '{ me { workspaces { projects { edges { node { id name environments { edges { node { id name } } } } } } } } }' \
  | python3 -c "import sys,json,os
d=json.load(sys.stdin); name=os.environ['PROJECT_NAME']; pid=env=''
for w in d['me']['workspaces']:
  for e in w['projects']['edges']:
    n=e['node']
    if n['name']==name:
      pid=n['id']
      for ee in n['environments']['edges']:
        if ee['node']['name']=='production': env=ee['node']['id']
print(pid, env)")

if [ -z "$PROJECT_ID" ] || [ -z "$ENV_ID" ]; then
  echo "ERRO: projeto '$PROJECT_NAME' ou environment production não encontrado" >&2
  exit 1
fi
export PROJECT_ID ENV_ID
log "PROJECT_ID=$PROJECT_ID ENV_ID(production)=$ENV_ID"

# serviço -> id (por nome)
SERVICES_JSON="$(gql 'query($id: String!){ project(id:$id){ services{ edges{ node{ id name } } } } }' \
  "$(python3 -c "import os,json;print(json.dumps({'id':os.environ['PROJECT_ID']}))")" \
  | python3 -c "import sys,json;print(json.dumps({e['node']['name']:e['node']['id'] for e in json.load(sys.stdin)['project']['services']['edges']}))")"

log "Redeployando serviços de app…"
rc=0
for name in $APP_SERVICES; do
  sid="$(printf '%s' "$SERVICES_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin).get(sys.argv[1],''))" "$name")"
  if [ -z "$sid" ]; then echo "  ! $name não existe no projeto — pulando"; continue; fi
  if gql 'mutation($e: String!, $s: String!){ serviceInstanceRedeploy(environmentId:$e, serviceId:$s) }' \
      "$(SID="$sid" python3 -c "import os,json;print(json.dumps({'e':os.environ['ENV_ID'],'s':os.environ['SID']}))")" >/dev/null; then
    echo "  ↻ $name"
  else
    echo "  ✗ $name FALHOU"; rc=1
  fi
done
exit $rc
