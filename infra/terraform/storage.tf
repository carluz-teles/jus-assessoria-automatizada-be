# storage.tf — bucket S3-compatível dos PDFs/payloads brutos.
#
# O Railway NÃO tem recurso de object storage, e o provider community não expõe um.
# O bucket vive FORA do Railway, num serviço S3-compatível (AWS S3, Cloudflare R2 ou
# MinIO). Provisionamos com o provider `aws` apontado para o endpoint S3-compatível —
# é universal (as mesmas 4 credenciais S3_* que a app consome em runtime). Trocar
# para R2 nativo é um recurso só (cloudflare_r2_bucket); a forma não muda.
#
# As credenciais são as variáveis sensíveis S3_* (cofre/CI), nunca commitadas.

provider "aws" {
  access_key = var.s3_access_key
  secret_key = var.s3_secret_key
  region     = var.s3_region

  # Serviço S3-compatível não-AWS: pula as checagens específicas da AWS e usa
  # path-style. Para AWS S3 real, deixe s3_endpoint vazio (endpoint padrão).
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  s3_use_path_style           = true

  endpoints {
    s3 = var.s3_endpoint
  }
}

resource "aws_s3_bucket" "documents" {
  bucket = var.s3_bucket
}

# Bucket privado — nada de PDF de processo exposto publicamente. O acesso é por
# presigned URL (docs §API), gerado pela app.
resource "aws_s3_bucket_public_access_block" "documents" {
  bucket = aws_s3_bucket.documents.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# Versionamento: protege contra sobrescrita/deleção acidental do payload bruto.
resource "aws_s3_bucket_versioning" "documents" {
  bucket = aws_s3_bucket.documents.id

  versioning_configuration {
    status = "Enabled"
  }
}

# Criptografia em repouso (AWS S3/MinIO). No R2 a criptografia é gerenciada pela
# Cloudflare; este recurso é inócuo/ignorado nesse alvo.
resource "aws_s3_bucket_server_side_encryption_configuration" "documents" {
  bucket = aws_s3_bucket.documents.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

output "documents_bucket" {
  description = "Nome do bucket S3-compatível de documentos."
  value       = aws_s3_bucket.documents.bucket
}
