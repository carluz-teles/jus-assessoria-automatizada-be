# ERD — Notificações em tempo real (in-app push)

> **Status:** proposta (v1) — transporte **SSE decidido**. Fonte de verdade de schema continua
> `erd-modelo-de-dados.md`; este doc detalha o subsistema de avisos em tempo real e como ele
> encaixa no que já existe.

## 1. Contexto & objetivo

Hoje a slice `internal/notifications` já é **multi-canal por design** (`notification` = o
fato "fulano precisa ser avisado de X"; `notification_delivery` = uma tentativa por canal —
a 0008 já prevê *"a future SMS/IN_APP channel is another delivery row"*). Só o canal **EMAIL**
existe. E os eventos de domínio `acquisition.backfill_finished` e `acquisition.docket_entry_observed`
são **produzidos sem consumer** — geram flood de `handler not found` (ver §9).

**Objetivo:** entregar avisos **in-app em tempo real** ao FE, cobrindo dois casos:
1. **Banner de importação** nas tabelas de Processos/Intimações: "estamos importando seus
   processos…" enquanto o backfill roda, some quando termina (e refetch das tabelas).
2. **Avisos incrementais** pós-import: nova intimação/andamento, membro entrou no escritório, etc.
   → sino (badge de não-lidas) + toast.

Isso resolve o flood **com semântica real**: a slice `notifications` passa a consumir esses
eventos e vira sua razão de existir.

## 2. Decisão de transporte — SSE vs WebSocket

O tráfego relevante é **server → client** (o servidor empurra avisos/progresso). As ações do
usuário (marcar lido) são REST comuns, não precisam do canal duplex.

| Critério | **SSE** (recomendado) | WebSocket |
|---|---|---|
| Direção | Unidirecional S→C (é o que precisamos) | Bidirecional (não usaríamos a volta) |
| Protocolo | HTTP puro (funciona com Fiber, proxies, Railway) | Upgrade + frame protocol |
| Reconexão | **Nativa** no browser (`EventSource`, `Last-Event-ID`) | Manual (ping/pong, backoff próprio) |
| Infra/escala | Sem sticky session; fan-out via Redis pub/sub | Costuma exigir sticky session / mais tuning |
| Lib no BE | `c.SendStreamWriter` (Fiber, zero dep extra) | `gofiber/contrib/websocket` |
| Auth no browser | Header não dá em `EventSource` → usar `@microsoft/fetch-event-source` (permite header + reconnect) ou token efêmero na query | Mesma limitação de header |
| Complexidade | Baixa | Média/alta |

**Recomendação: SSE.** Casa com o caso (push unidirecional), é HTTP-nativo, reconecta sozinho e
escala com o Redis que já temos. WebSocket fica reservado para quando surgir necessidade
**bidirecional de baixa latência** (chat, colaboração ao vivo) — não é o caso de avisos.

**Auth (cross-subdomain `app.atjud.com.br` → `api.atjud.com.br`):** o `EventSource` do browser
não seta `Authorization`. No FE usamos **`@microsoft/fetch-event-source`** (fetch + stream, aceita
header `Authorization: Bearer <clerk token>` e mantém reconexão). Alternativa sem lib: endpoint
`POST /v1/notifications/stream-token` que emite um JWT curto e o FE abre
`EventSource('/v1/notifications/stream?token=…')`. Preferir a primeira (menos peça móvel).

## 3. Arquitetura ponta a ponta

```
  PRODUTORES (outbox, já existem)                CONSUMER (notifications slice)
  ─────────────────────────────                 ──────────────────────────────
  acquisition.backfill_started  ─┐
  acquisition.backfill_finished ─┤   asynq       ┌─ mapeia evento → Notification (fato)
  acquisition.court_record_obs. ─┼──(ingestao)──▶│  + delivery IN_APP (persist, RLS por tenant)
  acquisition.docket_entry_obs. ─┤   queue        │  + suprime durante backfill (§6)
  identity.member_joined ────────┘                └─ PUBLISH no Redis: canal notif:{tenant}:{user}
                                                                    │
                                             ┌──────────────────────┴───────────────────┐
                                             ▼ (cada réplica da api assina o Redis)      ▼
                                       api réplica A                              api réplica B
                                       GET /v1/notifications/stream (SSE)  ...
                                             │  empurra `event: notification\n data: {…}`
                                             ▼
                                       FE  fetchEventSource ─▶ store ─▶ sino/badge + toast + banner
                                                                     └▶ React Query invalidate (refetch tabelas)
```

- **Fan-out multi-réplica:** o worker publica no **Redis pub/sub** (`notif:{tenant_id}` ou
  `:{user_id}`); cada instância da api assina e empurra para os `EventSource` conectados naquela
  instância. Sem sticky session.
- **Fonte de verdade = banco.** O Redis pub/sub é *fire-and-forget* (best-effort real-time). Quem
  estava offline pega tudo do `notification` no próximo load (lista + contagem de não-lidas). SSE
  nunca é a única via — logo, nada se perde. Entrega at-least-once + idempotência já existentes
  seguem valendo no consumer.

## 4. Modelo de dados (extensão do 0008, não reescrita)

`notification` + `notification_delivery` continuam. Mudanças mínimas para o canal in-app:

- **`notification_delivery.channel`** ganha o valor **`IN_APP`** (validado na app, como EMAIL).
  Uma notificação pode ter delivery EMAIL **e** IN_APP — dois rows, um aggregate.
- **Estado de leitura (in-app):** adicionar em `notification`:
  - `read_at timestamptz NULL` — marcado quando o usuário lê (só faz sentido com
    `recipient_user_id` preenchido; avisos tenant-level são informativos).
  - índice parcial p/ badge: `CREATE INDEX ON notification (recipient_user_id) WHERE read_at IS NULL;`
- **Render:** manter `type` (template selector) + `payload` (dados). O texto renderizado
  (título/corpo) sai do template no momento da entrega (como o EMAIL já faz), ou é materializado
  em `title`/`body` na notificação se quisermos histórico imutável — **decisão aberta** (recomendo
  materializar `title`/`body` para o in-app não depender de template versionado).

Migration nova (ex.: `00NN_notifications_in_app.up.sql`): `ALTER TABLE notification ADD read_at …`,
`ADD title text`, `ADD body text`, o índice parcial, e o CHECK/validação de channel aceitando `IN_APP`.

## 5. Eventos → notificação (mapa)

| Evento (produtor) | Vira notificação? | `type` | Destinatário | Canais |
|---|---|---|---|---|
| `identity.member_joined` | sim (já existe via `notification.requested`) | `member_joined` | membro que entrou | EMAIL (+ IN_APP) |
| `acquisition.backfill_started` | sim | `import_started` | tenant (ou quem ativou) | IN_APP (banner) |
| `acquisition.backfill_finished` | sim | `import_finished` | tenant | IN_APP (banner + toast) |
| `acquisition.court_record_observed` | **só incremental** (§6) | `new_processo` | tenant | IN_APP |
| `acquisition.docket_entry_observed` | **só incremental** (§6) | `new_andamento` | tenant | IN_APP |

`member_joined` pode continuar passando por `notification.requested` (o produtor decide avisar);
os eventos de acquisition são consumidos **direto** pela slice notifications (o produtor não
conhece a semântica de aviso — desacoplado).

## 6. Fluxos

### 6.1 Banner de importação (o caso pedido)
- `backfill_started` → notificação `import_started` (IN_APP) com `total_slices`, `integration_id`.
  FE mostra o **banner** "importando seus processos…" nas telas de Processos/Intimações.
- Enquanto o backfill roda, os `court_record_observed`/`docket_entry_observed` **NÃO** viram
  notificação individual (senão um import de 6.5k processos = milhares de toasts). O banner cobre.
- `backfill_finished` → notificação `import_finished` (status COMPLETED/PARTIAL, tallies). FE
  **esconde o banner**, dá um toast "importação concluída — N processos" e **invalida** as queries
  de Processos/Intimações (refetch).

**Como suprimir durante o backfill (v1, sem mudar schema de evento):** o consumer, ao receber
`court_record_observed`/`docket_entry_observed`, checa se há `backfill_job` **RUNNING** para o
tenant; se houver, cria/atualiza só o agregado do import (ou ignora para notificação) e não emite
aviso individual. *(Alternativa: taggear o evento com origem `backfill|incremental` — mais preciso,
exige mudar o payload; fica como evolução.)*

### 6.2 Incremental (pós-import)
- Scheduler re-poll acha andamento novo → enrichment → `docket_entry_observed` (sem backfill ativo)
  → notificação `new_andamento` → push IN_APP → sino +1 e toast. FE invalida a intimação/processo.

### 6.3 Conexão e catch-up
- FE conecta o stream ao logar; ao (re)conectar, a api manda um **snapshot** de não-lidas (ou o FE
  faz `GET /v1/notifications?unread=true`). `Last-Event-ID` cobre o gap curto de reconexão.

## 7. API (borda)

- `GET /v1/notifications/stream` — **SSE**, autenticado. Assina `notif:{tenant}:{user}` no Redis e
  faz stream. `text/event-stream`, heartbeat (comment `:\n`) a cada ~20s p/ manter viva.
- `GET /v1/notifications?cursor=&unread=` — histórico paginado (envelope padrão `{data, page}`).
- `GET /v1/notifications/unread-count` — badge do sino.
- `POST /v1/notifications/{id}/read` e `POST /v1/notifications/read-all` — marca lido (`read_at`).
- (opção sem fetch-event-source) `POST /v1/notifications/stream-token` — JWT curto p/ a query do EventSource.
- `GET /v1/acquisition/import-status` *(ou derivar do banner via notificação)* — estado do backfill
  ativo para o banner sobreviver a refresh de página (read model sobre `backfill_job`).

## 8. Frontend

- **Cliente SSE:** `@microsoft/fetch-event-source` com `Authorization: Bearer <clerk>`; reconecta
  com backoff; fecha no logout/unmount.
- **Store de notificações** (React Query + um store leve): lista, contagem não-lida, append no push.
- **UI:** sino no header (badge), painel/dropdown de notificações, **toast** (sonner) no push,
  **banner** de import nas tabelas (dirigido por `import_started/finished` ou `import-status`).
- **Invalidação:** no push de `new_*`/`import_finished`, `queryClient.invalidateQueries` das listas
  afetadas → a tabela atualiza sozinha.

## 9. Como isso resolve o flood (e o interino)

`backfill_finished` e `docket_entry_observed` hoje roteiam pela fila `ingestao` (prefixo
`acquisition`) e, **sem handler**, o asynq levanta `handler not found` e retenta 25× — flood de
ERROR (a nova observabilidade expôs). Ao a slice `notifications` **consumir** esses eventos, eles
passam a ter handler → sem flood, com semântica de verdade.

- **Interino** (enquanto o consumer real não sobe): dá para deployar o ack no-op
  (`drainUnconsumed`, já commitado em `fix/drain-unconsumed-acquisition-events`) para **parar a
  sangria agora**, e depois trocar o drain pelo handler real do notifications. Entrega
  at-least-once garante que eventos futuros não se perdem na troca.
- **Billing/identity** também têm eventos produzidos-sem-consumer, mas roteiam pra fila `default`
  (que ninguém consome) → ficam *pending* no Redis, **sem** flood (acumulam devagar — item separado).

## 10. Slices/arquivos afetados & faseamento

- **BE `internal/notifications`:** novos `listener` handlers (backfill_started/finished,
  court_record_observed, docket_entry_observed); canal `IN_APP` (persist + publish Redis);
  read models + handlers HTTP (stream SSE, list, unread-count, read). Migration `00NN_*`.
- **BE `lib`:** um pequeno `pubsub` sobre o Redis já existente (publish/subscribe por canal);
  o endpoint SSE na borda (Fiber `SendStreamWriter`) + auth.
- **FE `autojus-fe`:** cliente SSE, store, sino/painel, toast, banner de import, invalidações.

**Fases sugeridas:** (1) notifications consome os eventos + cria notificação + canal IN_APP
persistido **(já mata o flood)**; (2) Redis pub/sub + endpoint SSE + push real-time; (3) FE
(sino/toast/banner); (4) refinos (suppress por backfill via tag, read-all, preferências).
