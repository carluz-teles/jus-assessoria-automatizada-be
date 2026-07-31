# infra/terraform — provisionamento do court-legal (BE) na Railway

Substitui o `infra/railway/provision.sh`/`redeploy.sh`. Gerencia o projeto **court-legal** e
seus 8 serviços (postgres+volume, redis, api, 6 workers). O serviço **`web` (FE)** vive no
mesmo projeto mas é gerenciado pelo Terraform do **repo do FE** (state separado) — este
módulo NÃO o declara.

## Modelo (provado no sandbox 2026-07-30)

- **Converge-loop, não one-shot:** o backend da Railway é assíncrono/lossy (um apply pode ter
  falso sucesso — env some, serviço some). `apply.sh` re-aplica até `plan` limpar
  (`MAX_ATTEMPTS`). **Sem destroy-on-failure** (catastrófico em prod).
- **Throttle:** `-parallelism=1`.
- **Variáveis por serviço:** cada binário recebe só o que consome (base dos 5 `required` do
  `config.Load` + extras por serviço). Ver `local.service_vars` em `main.tf`.
- **Imagens versionadas:** `var.image_tag` (=`github.sha` em CI) fixa `jus-<svc>:<sha>`.

## Arquivos

| Arquivo | Papel |
|---|---|
| `versions.tf` `variables.tf` `main.tf` `outputs.tf` | a config |
| `_env.sh` | mapeia env do projeto → `TF_VAR_*` (fonte única; sourced pelos scripts) |
| `import.sh` | **adoção**: traz projeto+8 serviços existentes pro state (uma vez). Só escreve state |
| `apply.sh` | converge-loop serial |
| `destroy.sh` | ⚠️ derruba tudo (só p/ sandbox, NUNCA prod) |

## Ordem de adoção (uma vez)

```bash
cd infra/terraform
set -a; source .env.railway; set +a         # mesmos secrets do GitHub Actions (REAIS)
export RAILWAY_TOKEN=... RAILWAY_WORKSPACE_ID=27838c17-0a9b-4799-9c59-fab7c6dbff19

./import.sh          # traz court-legal + 8 serviços pro state (NÃO muda infra)
terraform plan       # revise: deve mostrar só as variable collections a criar +
                     # as vars novas de billing/notifications que o provision.sh não tinha
./apply.sh           # converge-loop: cria as collections, reconcilia (redeploya o que mudou)
```

Depois disso o deploy do dia-a-dia é o `apply.sh` com `IMAGE_TAG=github.sha` (no CD).

## ⚠️ ABERTO: onde vive o state remoto (necessário pro CI/CD)

State local NÃO serve pro CI (cada run começa do zero → tentaria recriar tudo). Precisa de
backend remoto compartilhado. Opções: Terraform Cloud (precedente no repo) ou backend
S3-compat no R2 (já usamos R2). **Decisão pendente** — o `import.sh`/`apply.sh` acima usam
state local até isso ser definido; a adoção real espera o backend.

## IDs (conta autosjus / workspace AutosJusAi's Projects)

- workspace `27838c17-0a9b-4799-9c59-fab7c6dbff19` · court-legal project `0f0790a9-235b-499d-af63-c8f83b5dba0b`
- env production `04d181f3-b54e-48ac-8804-2719fd76f525`
