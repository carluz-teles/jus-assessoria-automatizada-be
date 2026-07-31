# infra/terraform — STAGING do BE (court-legal-stg) na Railway

Cria um projeto Railway **court-legal-stg** do zero (separado do court-legal de produção),
com os 8 serviços (postgres+volume, redis, api, 6 workers), variable collections POR SERVIÇO,
e domínio auto-gerado no api. O serviço **`web` (FE)** vive no mesmo projeto stg mas é
gerenciado pelo Terraform do **repo do FE** (state separado).

## Por que projeto separado (e não env stg dentro do court-legal)

O provider community não isola `source_image` por-environment (é project-level) — um env stg
no mesmo projeto compartilharia a imagem com a produção. Projeto separado = **isolamento
total** (imagem, volume, dados, domínio) e **zero risco** pro court-legal de prod.

## Modelo (provado no sandbox)

- **Converge-loop:** o backend da Railway é async/lossy; `apply.sh` re-aplica até `plan` limpar
  (`MAX_ATTEMPTS`). **Sem destroy-on-failure.**
- **Throttle:** `-parallelism=1`.
- **Variáveis por serviço:** base dos 5 `required` do `config.Load` + extras por serviço.
- **Imagens versionadas:** `var.image_tag` (=`github.sha` em CI) fixa `jus-<svc>:<sha>`.

## State remoto (Terraform Cloud)

Org `Autojus`, workspace `autojus-terraform` (por env: `TF_CLOUD_ORGANIZATION`, `TF_WORKSPACE`).
Auth: `TF_TOKEN_app_terraform_io`. Terraform **1.15.8**.

## Rodar (local)

```bash
cd infra/terraform
set -a; source .env.railway; set +a          # secrets reais (os mesmos do GitHub Actions)
export RAILWAY_TOKEN=... RAILWAY_WORKSPACE_ID=27838c17-0a9b-4799-9c59-fab7c6dbff19
export TF_TOKEN_app_terraform_io=... TF_CLOUD_ORGANIZATION=Autojus
export IMAGE_TAG=$(git rev-parse HEAD)
./apply.sh          # converge-loop: cria o court-legal-stg e tudo dentro
```

Depois do 1º apply, pegue o output **`stg_environment_id`** e passe pro FE (GH var
`STG_ENVIRONMENT_ID`) — o web do FE deploya no mesmo environment.

## IDs

- workspace `27838c17-0a9b-4799-9c59-fab7c6dbff19` (conta autosjus) · o court-legal-stg é criado
  (id sai no output). O court-legal de PRODUÇÃO (`0f0790a9-…`) NÃO é tocado por este módulo.
