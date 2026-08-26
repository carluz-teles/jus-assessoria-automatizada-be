-- Bug de segurança: Sign() descartava a senha do request e nunca comparava com
-- a senha real do vault (envelope cifrado) — qualquer senha "funcionava". A
-- correção compara em domain.go; esta migration adiciona a política que decide
-- QUANDO a comparação é exigida. Default 'always' preserva o comportamento
-- correto pré-existente (senha sempre exigida) para todo certificado já
-- cadastrado.
ALTER TABLE certificate
  ADD COLUMN password_policy text NOT NULL DEFAULT 'always'
    CHECK (password_policy IN ('always', 'session', 'never'));
