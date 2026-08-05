# Code Review — sub-fatia 2b (commit 99b5e75)

Escopo: `git diff 4210396..HEAD`

---

## BLOCKING

### B1 · `lib/pubsub/pubsub.go:73` — erro de `sub.Close()` descartado com `_` — ✅ RESOLVIDO

```go
if _, err := sub.Receive(ctx); err != nil {
    _ = sub.Close()   // ← BLOCKING
    return nil, fmt.Errorf("subscribe to %s: %w", channel, err)
}
```

**Por quê é bloqueante.** A skill `golang-error-handling` é explícita: "Returned errors MUST always be checked — NEVER discard with `_`". Mesmo num cleanup path (já estamos retornando erro), o `_` é proibido — o close error deve ser combinado com o receive error via `errors.Join`. O `defer sub.Close()` na linha 80 (success path) é o padrão Go aceito para recursos read-only; `_ =` no error path não é.

**Correção esperada (não conserte aqui — só para clareza):**
```go
if _, err := sub.Receive(ctx); err != nil {
    return nil, errors.Join(fmt.Errorf("subscribe to %s: %w", channel, err), sub.Close())
}
```

---

## Veredito: `done=true`

B1 corrigido: `sub.Close()` no error path agora é combinado ao erro de `Receive` via
`errors.Join` (import `errors` adicionado). Nenhum blocking pendente.

---

## Advisory (não bloqueiam aprovação)

- **`lib/pubsub/pubsub.go:48`** — `NewRedisPubSub` retorna `*redisPubSub` (tipo não-exportado). Convenção Go: construtor exportado retorna tipo exportado ou interface. Como os callers injetam via `publisher`/`Subscriber` (interfaces), nada quebra agora; porém se um futuro caller precisar tipagem explícita no ponto de declaração, não consegue escrever o tipo. Alternativa: retornar `interface{ Publisher; Subscriber }` — ou exportar o tipo como `RedisPubSub`.

- **`internal/notifications/domain.go:295,341`** — o terceiro arg de `uc.publish(...)` é `TypeBackfillFinished` / `TypeDocketEntryObserved` (event type de aquisição, e.g. `"acquisition.backfill_finished"`), não o tipo de notificação. O parâmetro se chama `eventType` e aparece *somente* em atributos de log (`"event_type"`); o campo `Type` do `inAppPush` vem de `notif.Type` (`TypeImportFinished` / `TypeNewAndamento`). O código está correto, mas quem lê o callsite pode confundir o argumento com o tipo da push. Considerar renomear o parâmetro para `sourceEventType` para deixar a distinção explícita.
