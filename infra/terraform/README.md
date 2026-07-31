# infra/terraform — provisionamento do court-legal (BE, PRODUÇÃO) na Railway

Substitui o `infra/railway/provision.sh`/`redeploy.sh`. Gerencia o projeto **court-legal** e
seus 8 serviços (postgres+volume, redis, api, 6 workers), no environment **production**. O
serviço **`web` (FE)** vive no mesmo projeto mas é gerenciado pelo TF do **repo do FE** (state
separado). (Staging fica pra depois — por ora movemos só com prod.)

## Adoção sobre infra existente (uma vez)

O court-legal já existe (criado pelo provision.sh). Rode o `import.sh` UMA vez pra trazer
projeto+serviços pro state; as variable collections o `apply.sh` cria via upsert (reconcilia +
adiciona as vars de billing/notifications que faltavam). O domínio do api (auto-gerado pelo
provision.sh) NÃO é gerenciado aqui — fica como está (follow-up).

```bash
cd infra/terraform
set -a; source .env.railway; set +a          # secrets reais (os mesmos do GitHub Actions)
export RAILWAY_TOKEN=... RAILWAY_WORKSPACE_ID=27838c17-0a9b-4799-9c59-fab7c6dbff19
export TF_TOKEN_app_terraform_io=... TF_CLOUD_ORGANIZATION=Autojus
./import.sh          # traz court-legal + 8 serviços pro state (NÃO muda infra)
terraform plan       # deve mostrar só as collections a criar + as vars novas (0 destroy)
export IMAGE_TAG=$(git rev-parse HEAD)
./apply.sh           # converge-loop: cria as collections, reconcilia
```

Plan de adoção validado ao vivo: **7 to add (collections) · 1 change (description) · 0 destroy**.

## Modelo

- **Converge-loop:** Railway é async/lossy; `apply.sh` re-aplica até `plan` limpar. **Sem
  destroy-on-failure.** **Throttle:** `-parallelism=1`. **Variáveis por serviço.**
- **Imagens versionadas:** `var.image_tag` (=`github.sha` em CI).
- **State:** Terraform Cloud (org `Autojus`, workspace `autojus-terraform`), terraform **1.15.8**.

## IDs

- workspace `27838c17-0a9b-4799-9c59-fab7c6dbff19` (conta autosjus) · court-legal project
  `0f0790a9-235b-499d-af63-c8f83b5dba0b` · env production `04d181f3-b54e-48ac-8804-2719fd76f525`.
