# storage.tf — storage dos PDFs/payloads brutos: bucket Cloudflare R2 (fora do Terraform).
#
# O Railway não tem object storage e o provider community não expõe um. O bucket vive
# num serviço S3-compatível: um bucket Cloudflare R2 (free tier), criado UMA VEZ à mão
# no painel do R2. O Terraform NÃO gerencia o bucket — não há provider AWS aqui; ele só
# INJETA as env vars S3_* (ver variables_env.tf) que o cliente de presigned URL da app
# consome em runtime.
#
# Config do R2 (nas variáveis sensíveis S3_*, do cofre/CI, nunca commitadas):
#   S3_ENDPOINT = https://<account_id>.r2.cloudflarestorage.com
#   S3_REGION   = "auto"
#   S3_BUCKET / S3_ACCESS_KEY / S3_SECRET_KEY = bucket + credenciais do token R2.
#
# Bucket privado — nada de PDF de processo exposto; acesso só por presigned URL gerada
# pela app. Criptografia em repouso e durabilidade são gerenciadas pela Cloudflare.
