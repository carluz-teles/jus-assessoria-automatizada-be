#!/usr/bin/env bash
# infra/railway/cost_report.sh — rateio simples de custo de infra por unidade de uso IA.
#
# Fase 2 da análise de custo (fase 1 = ai_usage_event, custo por chamada OpenRouter).
# Railway não expõe custo em $ por métrica bruta (CPU/memória/rede vêm em unidades de
# recurso, não dólar — reinventar a tabela de preço da Railway seria frágil e duplicado).
# O único número em $ confiável que a API expõe é a fatura fechada do período
# (Customer.invoices via `me.workspaces[].customer.invoices`). Então o rateio é:
#
#   custo_por_unidade = fatura_railway_do_periodo / volume_ai_usage_event_no_mesmo_periodo
#
# Isso dá o custo de infra médio por chamada IA no período faturado — não um custo exato
# por chamada (infra é fixa, não por-requisição), mas o sinal que a fase 1 combinada com
# esta fase foi desenhada para produzir.
#
# Requer a Railway CLI já autenticada (`railway login` / `railway whoami`) — usa `railway
# api` pra falar com o GraphQL, sem reimplementar o transporte HTTP (diferente de
# provision.sh/redeploy.sh, que rodam em CI e por isso levam RAILWAY_TOKEN explícito).
# Env: DATABASE_URL (obrigatório, pra somar o volume de ai_usage_event).
# Uso:  ./infra/railway/cost_report.sh                 # última fatura fechada
#       ./infra/railway/cost_report.sh <invoice_id>    # fatura específica (ver --list)
#       ./infra/railway/cost_report.sh --list          # lista faturas disponíveis
set -euo pipefail

: "${DATABASE_URL:?DATABASE_URL obrigatório (pra somar o volume de ai_usage_event)}"
command -v psql >/dev/null || { echo "psql (postgresql-client) é obrigatório" >&2; exit 1; }
command -v railway >/dev/null || { echo "Railway CLI é obrigatória (railway login)" >&2; exit 1; }

gql() {
  railway api "$1"
}

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

log "Buscando faturas Railway…"
INVOICES_JSON="$(gql '{ me { workspaces { customer { invoices { invoiceId total status periodStart periodEnd } } } } }' \
  | python3 -c "
import sys, json
d = json.load(sys.stdin)['data']
invoices = []
for w in d['me']['workspaces']:
    c = w.get('customer')
    if c:
        invoices.extend(c['invoices'])
print(json.dumps(invoices))
")"

if [ "${1:-}" = "--list" ]; then
  echo "$INVOICES_JSON" | python3 -c "
import sys, json
for inv in json.load(sys.stdin):
    print(f\"{inv['invoiceId']}  {inv['status']:8s}  {inv['periodStart'][:10]} -> {inv['periodEnd'][:10]}  \${inv['total']/100:.2f}\")
"
  exit 0
fi

TARGET_ID="${1:-}"
read -r INVOICE_ID TOTAL_CENTS PERIOD_START PERIOD_END < <(echo "$INVOICES_JSON" | python3 -c "
import sys, json
invoices = json.load(sys.stdin)
target = sys.argv[1] if len(sys.argv) > 1 and sys.argv[1] else None
if target:
    matches = [i for i in invoices if i['invoiceId'] == target]
else:
    # a última fatura por periodEnd (a mais recente fechada)
    matches = sorted(invoices, key=lambda i: i['periodEnd'])[-1:]
if not matches:
    sys.stderr.write('nenhuma fatura encontrada\n'); sys.exit(1)
inv = matches[0]
print(inv['invoiceId'], inv['total'], inv['periodStart'], inv['periodEnd'])
" "$TARGET_ID")

TOTAL_USD="$(python3 -c "print(f'{$TOTAL_CENTS / 100:.2f}')")"
log "Fatura $INVOICE_ID — período $PERIOD_START -> $PERIOD_END — total \$$TOTAL_USD"

log "Somando volume de ai_usage_event no período…"
VOLUME_ROW="$(psql "$DATABASE_URL" -tA -F'|' -c "
  SELECT count(*), COALESCE(sum(cost_usd), 0)
  FROM ai_usage_event
  WHERE created_at >= '$PERIOD_START' AND created_at < '$PERIOD_END';
")"
IFS='|' read -r VOLUME LLM_COST_USD <<< "$VOLUME_ROW"

echo
echo "── Rateio de custo de infra por chamada IA ──────────────────────────"
echo "Período faturado Railway : $PERIOD_START -> $PERIOD_END"
echo "Custo Railway (infra)    : \$$TOTAL_USD"
echo "Chamadas IA no período   : $VOLUME"
echo "Custo OpenRouter (LLM)   : \$$LLM_COST_USD"
if [ "$VOLUME" -gt 0 ]; then
  python3 -c "
infra = $TOTAL_USD
llm = $LLM_COST_USD
volume = $VOLUME
print(f'Custo infra / chamada    : \${infra / volume:.6f}')
print(f'Custo total  / chamada   : \${(infra + llm) / volume:.6f}  (infra rateado + LLM real)')
"
else
  echo "Custo infra / chamada    : n/a (nenhuma chamada IA registrada no período)"
fi
echo "──────────────────────────────────────────────────────────────────"
