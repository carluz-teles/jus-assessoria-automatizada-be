-- 0060_draft_workflow_steps — persiste o step em que o usuário está no
-- peticionamento (Construção → Assinatura → Protocolo → Concluído).
--
-- Modelagem por FATOS DATADOS (não por coluna "current_step"). Cada etapa é
-- um timestamp nullable; a UI/BE deriva o step atual olhando qual fato mais
-- recente aconteceu. Vantagens: cada valor é auditável (quando aconteceu),
-- rollback é NULL do timestamp (sem "voltar coluna de step"), e não duplica
-- info do draft.status.
--
-- Regra de derivação:
--   sent_to_signing_at IS NULL                             → Construção
--   sent_to_signing_at ≠ NULL, signed_at IS NULL           → Assinatura
--   signed_at ≠ NULL, filed_at IS NULL                     → Protocolo
--   filed_at ≠ NULL                                        → Concluído
--
-- signed_at + filed_at JÁ EXISTIAM (0053 e via petition.filed_at). Este migration
-- adiciona apenas o gesto novo (sent_to_signing_at) + o número do protocolo
-- opcional (input manual do advogado quando marca "protocolada" — Fatia 2a v0;
-- integração PJe/e-SAJ é fatia futura).
ALTER TABLE draft
  ADD COLUMN sent_to_signing_at timestamptz,
  ADD COLUMN filing_number      text;

-- filed_at já existe em petition; expõe no draft por conveniência do read model
-- (evita join na hora de derivar o step). Sincronizado pelo use case quando
-- POST /file cria a petition.
ALTER TABLE draft
  ADD COLUMN filed_at timestamptz;
