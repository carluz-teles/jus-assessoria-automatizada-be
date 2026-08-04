# Code Review — DJEN Connector (sub-fatias A→D)

Diff base: `4b450ad..HEAD`  
Green gate: `go build ./... && go test ./...` — **exit 0** ✓

---

## BLOCKER-01 — Connector-parser payload format mismatch (hard break in production) — ✅ RESOLVIDO

**Fix aplicado (Dev)**

O achado estava correto: o conector marshalava um array flat e o parser esperava o envelope `{scope, items}`; além disso o `scope` nunca era preenchido, corrompendo `recipients.matched`.

- `internal/acquisition/djen_connector.go` (`Fetch`): agora deriva `scope` de `req.OABs` e marshala um novo tipo `djenAggregate{Scope djenScope; Items []json.RawMessage}` em vez do array flat. O JSON produzido é byte-idêntico ao que o parser decodifica em `djenEnvelope`.
  - **Reuse-check (Regra nº1):** o conector **REUSA** `djenScope`/`djenOAB` já declarados no `djen_parser.go` (mesmo pacote `acquisition`) — fonte única do contrato do scope. Diverge do exemplo do achado (que sugeria um `djenPayload` com o scope re-declarado "sem importar tipos do parser"): como estão no mesmo pacote, re-declarar o scope seria duplicação/bug de design. `Items` continua `[]json.RawMessage` (o conector carrega bytes verbatim, não conhece o schema do item).
- `internal/acquisition/djen_connector_test.go`:
  - `aggregatedItems` atualizado para desempacotar `djenAggregate` e retornar `.Items` (o body agora é objeto, não array) — os testes de contagem/paginação seguem verdes.
  - **Novo `TestDJENConnector_Fetch_RoundTripsThroughParser`** fecha a lacuna apontada: um httptest server retorna uma comunicação real, o conector faz `Fetch`, o body é decodificado como o `djenEnvelope` real (prova o formato + o `scope` com os OABs do request) e o **parser real** faz `Parse` — asserindo que o advogado no escopo volta `matched: true` (cobre a causa secundária). Passa com `-race`.

Green gate: `go build ./... && go test ./...` — **exit 0** ✓ (round-trip verde inclusive sob `-race`).

---

## BLOCKER-01 (original) — Connector-parser payload format mismatch (hard break in production)

**Files**
- `internal/acquisition/djen_connector.go:117–136` (`Fetch`)
- `internal/acquisition/djen_parser.go:88–113` (`Parse` / `djenEnvelope`)

### O que está errado

O conector produz um payload cujo `Body` é um **JSON array flat**:

```go
// djen_connector.go:117
items := []json.RawMessage{}
// ... itens coletados do loop de páginas ...
body, err := json.Marshal(items)   // → [{"hash":"...",...}, ...]
```

O parser espera um **JSON object** com os campos `scope` e `items` (o `djenEnvelope`):

```go
// djen_parser.go:88
var env djenEnvelope
if err := json.Unmarshal(payload.Body, &env); err != nil {
    return ParsedResult{}, fmt.Errorf("djen parser: unmarshal payload: %w", err)
}
```

```go
// djen_parser.go:308
type djenEnvelope struct {
    Scope djenScope  `json:"scope"`
    Items []djenItem `json:"items"`
}
```

`json.Unmarshal(<JSON array>, <*struct>)` retorna `*json.UnmarshalTypeError` (array into Go value of type acquisition.djenEnvelope). O `if err != nil` do parser captura o erro e retorna falha de parse. Em produção, **todo sync DJEN falha com `asynq.SkipRetry`** — o task é arquivado sem reprocessamento.

### Consequência secundária (mesma causa raiz)

Mesmo que o formato fosse corrigido, o conector **nunca inclui o scope** no body. O campo `req.OABs` é usado apenas como parâmetro de busca; o `djenEnvelope.Scope.OABs` jamais é preenchido. Em consequência:

```go
// djen_parser.go:95
scope := newOABSet(env.Scope.OABs) // sempre vazio
```

O `oabSet` retornado é vazio → `scope.has(...)` retorna sempre `false` → todos os advogados ficam com `matched: false` independentemente do escopo da integração. O campo `recipients.matched` fica corrompido.

### Por que os testes não pegaram

- **Teste do conector** (`TestDJENConnector_Fetch_*`): valida o body como `[]json.RawMessage` via `aggregatedItems` — correto para o que o conector produz, mas não valida o contrato com o parser.
- **Testes do parser** (`TestDJENParser_Parse_*`): constroem o payload manualmente com `wireEnvelope{Scope:..., Items:...}` — correto para o que o parser espera, mas não usa a saída real do conector.
- **Nenhum teste valida o round-trip** `Connector.Fetch → Parser.Parse`.

### Fix esperado

O conector deve produzir a estrutura que `djenEnvelope` declara — incluindo o scope derivado de `req.OABs`. Exemplo de estrutura local para o marshal no conector (sem importar tipos do parser):

```go
type djenPayload struct {
    Scope struct {
        OABs []struct {
            Number string `json:"number"`
            UF     string `json:"uf"`
        } `json:"oabs"`
    } `json:"scope"`
    Items []json.RawMessage `json:"items"`
}
```

Deve ser acompanhado de um teste de round-trip (`Fetch` → `Parse`) que garanta que `env.Scope.OABs` recebe os OABs do request e que `env.Items` é decodificável como `[]djenItem`.
