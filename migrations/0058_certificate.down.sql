-- Reverte 0058: drop da tabela certificate. Destrutivo — todo cadastro é perdido
-- (o binário no object storage tb fica órfão, precisa GC manual quando aplicado).
DROP TABLE IF EXISTS certificate;
