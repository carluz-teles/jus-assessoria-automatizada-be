-- 0079_court_fetch_state — progresso incremental de FetchAutos por
-- (court_connection, court_record): "o que já buscamos e até onde" no fluxo de
-- leitura autenticada do eproc/TJSP.
--
-- `court_record`/`docket_entry` são donos de `internal/acquisition` — este slice
-- (`internal/court`) nunca escreve neles nem os importa por entity/repo (regra de
-- dependência entre slices), então court_record_id é uma FK LÓGICA (sem
-- REFERENCES físico) resolvida por evento (court_record_observed/
-- intimation.observed), não por join direto de schema.
--
-- observed_at é o carimbo do evento mais recente que o listener viu pra esse
-- registro — é o que define "devido" (last_fetched_at NULL OU < observed_at), sem
-- precisar de JOIN contra court_record: o listener é quem decide relevância (evento
-- chegou + existe court_connection casando) e grava aqui, então a query de "o que
-- falta buscar" fica inteira dentro da tabela do próprio slice.
--
-- cnj_number (denormalizado do payload do evento, pelo mesmo motivo de
-- court_record_id ser FK lógica): o fetch de verdade precisa do número CNJ pra
-- chamar o provider — sem guardar aqui, precisaria de JOIN contra court_record só
-- pra resolver isso, o que este slice não faz.
--
-- tenant_id é denormalizado de court_connection (não normativo — court_connection
-- já garante 1 tenant por conexão) só pra manter a convenção de isolamento em 2
-- barreiras (filtro na app + RLS) igual toda tabela de usuário do schema.

CREATE TABLE court_fetch_state (
  tenant_id            uuid NOT NULL REFERENCES tenant(id),
  court_connection_id  uuid NOT NULL REFERENCES court_connection(id),
  court_record_id      uuid NOT NULL,
  cnj_number            text NOT NULL,
  observed_at           timestamptz NOT NULL,
  last_fetched_at       timestamptz,
  docket_cursor          timestamptz,
  PRIMARY KEY (court_connection_id, court_record_id)
);
CREATE INDEX ON court_fetch_state (tenant_id);
CREATE INDEX ON court_fetch_state (court_connection_id, last_fetched_at);

ALTER TABLE court_fetch_state ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON court_fetch_state
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);
