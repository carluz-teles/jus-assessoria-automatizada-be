-- 0059_certificate_envelope — troca o modelo de storage do binário .pfx:
-- de {storage_key → R2 blob} pra {ciphertext + envelope KMS in-row bytea}.
--
-- Motivação: passamos a usar GCP Cloud KMS pra "wrappar" a chave de dados
-- (envelope encryption). O KMS nunca vê o .pfx (só uma DEK de 32B); o próprio
-- api gera a DEK aleatória, cifra o .pfx com ela localmente (AES-256-GCM),
-- e chama KMS.Encrypt UMA vez pra cifrar a DEK. Guardamos ciphertext + nonce
-- do AES local + wrapped_dek (bytes do KMS) + kek_ref (nome da key do KMS
-- pra suportar rotate/multi-key no futuro). Sem KMS master key, sem forma
-- de recuperar a DEK — atacante com dump do PG não abre o .pfx.
--
-- Bônus: dropamos a dependência do object storage pra certificados. .pfx
-- é pequeno (< 100KB tipicamente), cabe confortavelmente in-row bytea.
-- Isso simplifica ops (backup do DB já leva os certs) e elimina o custo
-- extra do S3/R2 pra esse subsistema. Documentos grandes continuam no
-- storage; certificados voltam pro PG.
--
-- Tabela vazia no ambiente atual → migração destrutiva sem backfill.
ALTER TABLE certificate
  DROP COLUMN storage_key,
  ADD COLUMN ciphertext  bytea NOT NULL,
  ADD COLUMN nonce       bytea NOT NULL,     -- 12 bytes do AES-GCM local
  ADD COLUMN wrapped_dek bytea NOT NULL,     -- output do KMS.Encrypt
  ADD COLUMN kek_ref     text  NOT NULL;     -- KMS resource name (pra rotate)
