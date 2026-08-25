-- Reverte 0059: volta pra storage_key. Destrutivo — todo cadastro é perdido
-- (os blobs no PG bytea eram cifrados por DEK+KMS; sem re-cifrar, não migram
-- pro storage). Aceito porque a rollback só faz sentido antes de haver dado real.
ALTER TABLE certificate
  DROP COLUMN ciphertext,
  DROP COLUMN nonce,
  DROP COLUMN wrapped_dek,
  DROP COLUMN kek_ref,
  ADD COLUMN storage_key text NOT NULL;
