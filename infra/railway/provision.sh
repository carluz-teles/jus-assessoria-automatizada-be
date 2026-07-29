#!/usr/bin/env bash
# infra/railway/provision.sh — provisiona a infra no Railway via GraphQL API.
#
# Substitui o Terraform (o provider community v0.6.2 tinha bugs demais: volume
# "inconsistent result", region drift p/ sfo, rate limit de deploy, colisão de env,
# 404 no refresh). Aqui falamos direto com a API do Railway — idempotente.
#
# Fluxo: find-or-create do projeto -> environment production (default) -> serviços
# (postgres, redis, 6 de app) -> variáveis por serviço (variableCollectionUpsert com
# skipDeploys, 0 redeploy) -> volume do Postgres -> 1 deploy por serviço no fim.
#
# TODA variável de ambiente e segredo vem por env var (as MESMAS do GitHub Actions).
# Uso local:  set -a; source .env.railway; set +a; ./infra/railway/provision.sh
# No CI: um job injeta os secrets como env e roda este script.
set -euo pipefail

# ===== 1) Config e segredos (env vars) =====
: "${RAILWAY_TOKEN:?RAILWAY_TOKEN obrigatório (Account/Team token)}"
: "${RAILWAY_WORKSPACE_ID:?RAILWAY_WORKSPACE_ID obrigatório}"
: "${POSTGRES_PASSWORD:?}"
: "${CLERK_SECRET_KEY:?}"
: "${CLERK_WEBHOOK_SECRET:?}"
: "${S3_ENDPOINT:?}"; : "${S3_REGION:?}"; : "${S3_BUCKET:?}"; : "${S3_ACCESS_KEY:?}"; : "${S3_SECRET_KEY:?}"
# Opcionais (podem ser vazios, mas a struct Config os exige PRESENTES):
export CLERK_ISSUER="${CLERK_ISSUER:-}"
export ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY:-}"
export OTEL_EXPORTER_OTLP_ENDPOINT="${OTEL_EXPORTER_OTLP_ENDPOINT:-}"
export OTEL_EXPORTER_OTLP_HEADERS="${OTEL_EXPORTER_OTLP_HEADERS:-}"
# Config não-secreta:
export PROJECT_NAME="${PROJECT_NAME:-court-legal}"
export POSTGRES_USER="${POSTGRES_USER:-jus}"
export POSTGRES_DB="${POSTGRES_DB:-jus}"
export IMAGE_REGISTRY="${IMAGE_REGISTRY:-ghcr.io/carluz-teles}"
export IMAGE_TAG="${IMAGE_TAG:-latest}"

export API="https://backboard.railway.app/graphql/v2"
export PG_MOUNT="/var/lib/postgresql/data"

# Serviços de app (nome = binário cmd/ = imagem jus-<nome>). postgres/redis à parte.
APP_SERVICES=(api worker-ingestao worker-documents worker-ai worker-outbox-relay scheduler)

# ===== 2) Helper GraphQL: gql <query> [variables_json] -> imprime data (JSON) =====
gql() {
  local __vars="${2:-}"; [ -n "$__vars" ] || __vars='{}'
  RAILWAY_QUERY="$1" RAILWAY_VARS="$__vars" python3 - <<'PY'
import os, json, urllib.request, urllib.error, sys
body = json.dumps({"query": os.environ["RAILWAY_QUERY"],
                   "variables": json.loads(os.environ["RAILWAY_VARS"])}).encode()
req = urllib.request.Request(os.environ["API"], data=body, headers={
    "Authorization": "Bearer " + os.environ["RAILWAY_TOKEN"],
    "Content-Type": "application/json",
    # Railway fica atrás do Cloudflare, que bloqueia o UA padrão do urllib (erro 1010).
    "User-Agent": "curl/8.5.0"})
try:
    r = json.load(urllib.request.urlopen(req, timeout=45))
except urllib.error.HTTPError as e:
    sys.stderr.write("HTTP %s: %s\n" % (e.code, e.read().decode()[:500])); sys.exit(1)
if r.get("errors"):
    sys.stderr.write("GQL ERROR: " + json.dumps(r["errors"])[:500] + "\n"); sys.exit(1)
print(json.dumps(r["data"]))
PY
}
# jpath '<expr>' lê JSON do stdin e imprime data<expr> (ex.: jpath "['project']['id']")
jpath() { python3 -c "import sys,json; d=json.load(sys.stdin); print(eval('d'+sys.argv[1]))" "$1"; }

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

# ===== 3) Mapas de variáveis por serviço (JSON), montados com urlencode da senha =====
build_env_json() {
  python3 - <<'PY'
import os, json, urllib.parse
enc = urllib.parse.quote(os.environ["POSTGRES_PASSWORD"], safe="")
database_url = f"postgres://{os.environ['POSTGRES_USER']}:{enc}@postgres.railway.internal:5432/{os.environ['POSTGRES_DB']}?sslmode=disable"
app = {
    "DATABASE_URL": database_url,
    "REDIS_URL": "redis://redis.railway.internal:6379/0",
    "CLERK_SECRET_KEY": os.environ["CLERK_SECRET_KEY"],
    "CLERK_WEBHOOK_SECRET": os.environ["CLERK_WEBHOOK_SECRET"],
    "CLERK_ISSUER": os.environ["CLERK_ISSUER"],
    "ANTHROPIC_API_KEY": os.environ["ANTHROPIC_API_KEY"],
    "OTEL_EXPORTER_OTLP_ENDPOINT": os.environ["OTEL_EXPORTER_OTLP_ENDPOINT"],
    "OTEL_EXPORTER_OTLP_HEADERS": os.environ["OTEL_EXPORTER_OTLP_HEADERS"],
    "APP_ENV": "production",
    "PORT": "8080",
    "S3_ENDPOINT": os.environ["S3_ENDPOINT"],
    "S3_REGION": os.environ["S3_REGION"],
    "S3_BUCKET": os.environ["S3_BUCKET"],
    "S3_ACCESS_KEY": os.environ["S3_ACCESS_KEY"],
    "S3_SECRET_KEY": os.environ["S3_SECRET_KEY"],
}
pg = {
    "POSTGRES_USER": os.environ["POSTGRES_USER"],
    "POSTGRES_PASSWORD": os.environ["POSTGRES_PASSWORD"],
    "POSTGRES_DB": os.environ["POSTGRES_DB"],
    # O volume monta em /var/lib/postgresql/data (com lost+found); initdb recusa um
    # datadir não-vazio. PGDATA aponta para um subdiretório do mount (recomendação
    # oficial do initdb), então o Postgres inicializa dentro do volume sem colisão.
    "PGDATA": "/var/lib/postgresql/data/pgdata",
}
print(json.dumps({"app": app, "postgres": pg}))
PY
}
ENV_JSON="$(build_env_json)"
APP_VARS="$(printf '%s' "$ENV_JSON" | python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin)["app"]))')"
PG_VARS="$(printf  '%s' "$ENV_JSON" | python3 -c 'import sys,json;print(json.dumps(json.load(sys.stdin)["postgres"]))')"

# ===== 4) Projeto: find-or-create no workspace =====
log "Procurando projeto '$PROJECT_NAME' no workspace…"
PROJECT_ID="$(gql '{ me { workspaces { id projects { edges { node { id name } } } } } }' \
  | python3 -c "import sys,json,os
d=json.load(sys.stdin); wid=os.environ['RAILWAY_WORKSPACE_ID']; name=os.environ['PROJECT_NAME']
pid=''
for w in d['me']['workspaces']:
  if w['id']==wid:
    for e in w['projects']['edges']:
      if e['node']['name']==name: pid=e['node']['id']
print(pid)")"

if [ -z "$PROJECT_ID" ]; then
  log "Criando projeto…"
  PROJECT_ID="$(gql 'mutation($i: ProjectCreateInput!){ projectCreate(input:$i){ id } }' \
    "$(python3 -c "import os,json;print(json.dumps({'i':{'name':os.environ['PROJECT_NAME'],'workspaceId':os.environ['RAILWAY_WORKSPACE_ID'],'defaultEnvironmentName':'production'}}))")" \
    | jpath "['projectCreate']['id']")"
fi
export PROJECT_ID
log "PROJECT_ID=$PROJECT_ID"

# environment production (default) + serviços/volumes existentes
read -r ENV_ID SERVICES_JSON VOLUMES_JSON < <(gql 'query($id: String!){ project(id:$id){ environments{ edges{ node{ id name } } } services{ edges{ node{ id name } } } volumes{ edges{ node{ id name } } } } }' \
  "$(python3 -c "import os,json;print(json.dumps({'id':os.environ['PROJECT_ID']}))")" \
  | python3 -c "import sys,json
d=json.load(sys.stdin)['project']
env=''
for e in d['environments']['edges']:
  if e['node']['name']=='production': env=e['node']['id']
svc={e['node']['name']:e['node']['id'] for e in d['services']['edges']}
vol={e['node']['name']:e['node']['id'] for e in d['volumes']['edges']}
print(env, json.dumps(svc), json.dumps(vol))")
export ENV_ID
log "ENV_ID(production)=$ENV_ID"

svc_id() { printf '%s' "$SERVICES_JSON" | python3 -c "import sys,json;print(json.load(sys.stdin).get(sys.argv[1],''))" "$1"; }

# ===== 5) find-or-create de um serviço. Args: <nome> <imagem>. Ecoa o serviceId =====
ensure_service() {
  local name="$1" image="$2" sid
  sid="$(svc_id "$name")"
  if [ -z "$sid" ]; then
    sid="$(gql 'mutation($i: ServiceCreateInput!){ serviceCreate(input:$i){ id } }' \
      "$(NAME="$name" IMAGE="$image" python3 -c "import os,json;print(json.dumps({'i':{'projectId':os.environ['PROJECT_ID'],'environmentId':os.environ['ENV_ID'],'name':os.environ['NAME'],'source':{'image':os.environ['IMAGE']}}}))")" \
      | jpath "['serviceCreate']['id']")"
    printf '  + criado %s (%s)\n' "$name" "$sid" >&2
  else
    printf '  = existe %s (%s)\n' "$name" "$sid" >&2
  fi
  printf '%s' "$sid"
}

# ===== 6) upsert de variáveis num serviço (skipDeploys). Args: <serviceId> <vars_json> =====
set_vars() {
  local sid="$1" vars="$2"
  gql 'mutation($i: VariableCollectionUpsertInput!){ variableCollectionUpsert(input:$i) }' \
    "$(SID="$sid" VARS="$vars" python3 -c "import os,json;print(json.dumps({'i':{'projectId':os.environ['PROJECT_ID'],'environmentId':os.environ['ENV_ID'],'serviceId':os.environ['SID'],'variables':json.loads(os.environ['VARS']),'replace':True,'skipDeploys':True}}))")" \
    >/dev/null
}

log "Criando/garantindo Postgres e Redis…"
PG_ID="$(ensure_service postgres 'pgvector/pgvector:pg16')"
REDIS_ID="$(ensure_service redis 'redis:7-alpine')"
export PG_ID

log "Criando/garantindo os ${#APP_SERVICES[@]} serviços de app…"
declare -A APP_IDS
for s in "${APP_SERVICES[@]}"; do
  APP_IDS[$s]="$(ensure_service "$s" "$IMAGE_REGISTRY/jus-$s:$IMAGE_TAG")"
done

log "Setando variáveis (skipDeploys)…"
set_vars "$PG_ID" "$PG_VARS"
for s in "${APP_SERVICES[@]}"; do set_vars "${APP_IDS[$s]}" "$APP_VARS"; done

# ===== 7) Volume do Postgres (find-or-create) =====
if printf '%s' "$VOLUMES_JSON" | python3 -c "import sys,json;sys.exit(0 if json.load(sys.stdin) else 1)"; then
  log "Volume já existe — pulando."
else
  log "Criando volume do Postgres em $PG_MOUNT…"
  gql 'mutation($i: VolumeCreateInput!){ volumeCreate(input:$i){ id } }' \
    "$(python3 -c "import os,json;print(json.dumps({'i':{'projectId':os.environ['PROJECT_ID'],'environmentId':os.environ['ENV_ID'],'serviceId':os.environ['PG_ID'],'mountPath':os.environ['PG_MOUNT']}}))")" \
    >/dev/null && echo "  + volume criado"
fi

# ===== 8) Deploy final: 1 redeploy por serviço (já com as variáveis) =====
log "Disparando deploy de cada serviço…"
redeploy() {
  gql 'mutation($e: String!, $s: String!){ serviceInstanceRedeploy(environmentId:$e, serviceId:$s) }' \
    "$(SID="$1" python3 -c "import os,json;print(json.dumps({'e':os.environ['ENV_ID'],'s':os.environ['SID']}))")" >/dev/null \
    && echo "  ↻ $2"
}
redeploy "$PG_ID" postgres
redeploy "$REDIS_ID" redis
for s in "${APP_SERVICES[@]}"; do redeploy "${APP_IDS[$s]}" "$s"; done

log "Pronto. Projeto '$PROJECT_NAME' provisionado no environment production."
