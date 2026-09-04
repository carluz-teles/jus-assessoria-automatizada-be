package events

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/obs"
)

// selectUnpublished drains one batch of undelivered outbox rows, interactive
// (priority 0) rows ahead of background ones (priority 1), FIFO within each
// priority. ORDER BY priority, id is satisfied directly by the composite partial
// index outbox_unpublished_priority_idx (priority, id) WHERE published_at IS NULL —
// an index-order scan, no sort — so a 10k-row bulk backfill (acquisition.*,
// priority 1) never delays an interactive row (draft.generation_requested, …)
// behind a full table sort. FOR UPDATE SKIP LOCKED lets several relay replicas
// share the table without publishing a row twice; the LIMIT bounds the work per
// Tick. Migration 0075.
const selectUnpublished = `SELECT id, type, payload, idempotency_key, trace_context, aggregate_id, process_at
	FROM outbox
	WHERE published_at IS NULL
	ORDER BY priority, id
	LIMIT 200
	FOR UPDATE SKIP LOCKED`

// markPublished stamps a row delivered. Issued only after its enqueue succeeds, in
// the same tx as the read, so a crash before it republishes the row next Tick.
const markPublished = `UPDATE outbox SET published_at = now() WHERE id = $1`

// Enqueuer is the slice of *asynq.Client the relay uses. The seam keeps Tick
// testable with a fake recorder — no Redis — and *asynq.Client satisfies it.
type Enqueuer interface {
	EnqueueContext(ctx context.Context, task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error)
}

// Relay is the only component that publishes to asynq. Tick drains the outbox in a
// single system-level transaction and marks each row published only after its
// enqueue succeeds (docs erd-backend §4c.2).
type Relay struct {
	uow database.UnitOfWork
	enq Enqueuer
}

// NewRelay wires the relay to its unit of work and enqueuer.
func NewRelay(uow database.UnitOfWork, enq Enqueuer) *Relay {
	return &Relay{uow: uow, enq: enq}
}

// pendingEvent is one outbox row read for publication. processAt is nil for the
// immediate rows (the default) and carries the ETA for a row that opted into future
// delivery; publish turns it into an asynq.ProcessAt option.
type pendingEvent struct {
	id             int64
	typ            string
	payload        []byte
	idempotencyKey string
	traceContext   string
	aggregateID    string
	processAt      *time.Time
}

// PublishedEvent is the identity of one event the relay drained this tick — the
// dimensions its logging needs (which events went out, under which trace), never
// the payload. The relay returns these so the loop can log a per-type summary and
// a per-event DEBUG line correlated to the event's own trace.
type PublishedEvent struct {
	Type         string // dotted event type (the asynq task type)
	ID           string // idempotency key = the event id
	AggregateID  string // the aggregate the event is about
	TraceContext string // W3C traceparent of the producing span
}

// Tick drains one batch of unpublished outbox rows into asynq and returns what it
// published. It runs in one transaction with an empty tenantID: the relay is
// system-level and reads across tenants (the outbox carries no tenant scope).
//
// For each row it enqueues the task, then marks the row published in the SAME tx.
// If an enqueue fails, Tick returns the error so the tx rolls back and nothing in
// this batch is marked — the rows are retried next Tick. That is the at-least-once
// contract: a crash between enqueue and mark republishes, and the consumer dedups
// (docs erd-backend §4c.2, §4c.3).
//
// A non-empty batch runs under a PRODUCER span so the per-row UPDATEs correlate
// and the outbox→publish drain is visible; idle ticks open no span (no one empty
// span per second).
func (r *Relay) Tick(ctx context.Context) (published []PublishedEvent, err error) {
	err = r.uow.Do(ctx, "", func(tx database.Tx) error {
		batch, err := readPending(ctx, tx)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		spanCtx, span := obs.Start(ctx, "outbox relay publish",
			trace.WithSpanKind(trace.SpanKindProducer),
			trace.WithAttributes(attribute.Int("batch.size", len(batch))),
		)
		defer span.End()

		for _, ev := range batch {
			if err := r.publish(spanCtx, tx, ev); err != nil {
				obs.Record(span, err)
				return err
			}
			published = append(published, PublishedEvent{
				Type:         ev.typ,
				ID:           ev.idempotencyKey,
				AggregateID:  ev.aggregateID,
				TraceContext: ev.traceContext,
			})
		}
		obs.Record(span, nil)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return published, nil
}

// readPending loads the batch and closes the rows before any further query on tx
// runs — pgx forbids a second query while a Rows is open on the same connection, so
// the whole batch is materialized before the per-row UPDATEs begin.
func readPending(ctx context.Context, tx database.Tx) ([]pendingEvent, error) {
	rows, err := tx.Query(ctx, selectUnpublished)
	if err != nil {
		return nil, database.WrapInfra(err)
	}
	defer rows.Close()

	var batch []pendingEvent
	for rows.Next() {
		var ev pendingEvent
		if err := rows.Scan(
			&ev.id, &ev.typ, &ev.payload,
			&ev.idempotencyKey, &ev.traceContext, &ev.aggregateID, &ev.processAt,
		); err != nil {
			return nil, database.WrapInfra(err)
		}
		batch = append(batch, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, database.WrapInfra(err)
	}
	return batch, nil
}

// publish enqueues one event — once PER destination queue (fan-out) — and marks its
// outbox row published. The traceparent travels as an asynq task header so the worker
// continues the producer's trace; TaskID gives asynq best-effort enqueue dedup, MaxRetry
// and Queue route by type.
//
// A single-consumer event (the common case) fans out to exactly ONE queue, so this is a
// straight enqueue, unchanged. A multi-consumer event (docket_entry_observed) fans out to
// several queues; asynq's TaskID uniqueness is GLOBAL (one task per id across the whole
// instance, per the asynq Unique-Tasks docs), so the same event id enqueued to two queues
// would collide and the 2nd copy would be dropped as ErrTaskIDConflict. The TaskID is
// therefore SUFFIXED by the queue ("<eventID>:<queue>") so each fan-out copy is a distinct
// asynq task — one per consuming server — while the payload event id (the consumer dedup
// key in processed_event) stays identical, so each consumer still dedups its own copy.
//
// ErrTaskIDConflict is not a failure: asynq already holds a task with this id, so that copy
// IS enqueued — treat it as delivered. The row is marked published only after ALL its copies
// are enqueued; any other enqueue error propagates, rolling the batch back for a later retry.
func (r *Relay) publish(ctx context.Context, tx database.Tx, ev pendingEvent) error {
	// Carry the event identity on the task so the consumer middleware can attribute
	// its span/log without decoding the payload; traceparent joins the consumer span
	// to the producer's (empty when the producer ran with no active span). The event id
	// on the header stays the UNSUFFIXED id (the consumer's dedup key), regardless of the
	// per-queue TaskID suffix below.
	headers := map[string]string{
		eventIDHeader:     ev.idempotencyKey,
		aggregateIDHeader: ev.aggregateID,
	}
	if ev.traceContext != "" {
		headers[traceparentKey] = ev.traceContext
	}

	for _, queue := range queuesFor(ev.typ) {
		task := asynq.NewTaskWithHeaders(ev.typ, ev.payload, headers)

		opts := []asynq.Option{
			asynq.Queue(queue),
			asynq.MaxRetry(maxRetryFor(ev.typ)),
		}
		if ev.idempotencyKey != "" {
			opts = append(opts, asynq.TaskID(taskIDForQueue(ev.idempotencyKey, queue)))
		}
		// An opted-in ETA becomes an asynq.ProcessAt option, so the task lands SCHEDULED
		// and the consumer only sees it once the time arrives. A nil processAt (the common
		// case) leaves opts untouched — the task is pending immediately, exactly as before.
		if ev.processAt != nil {
			opts = append(opts, asynq.ProcessAt(*ev.processAt))
		}

		if _, err := r.enq.EnqueueContext(ctx, task, opts...); err != nil {
			if !errors.Is(err, asynq.ErrTaskIDConflict) {
				return database.WrapInfra(err)
			}
		}
	}

	if _, err := tx.Exec(ctx, markPublished, ev.id); err != nil {
		return database.WrapInfra(err)
	}
	return nil
}

// taskIDForQueue makes the asynq TaskID unique PER destination queue so a fan-out event
// enqueued to N queues yields N distinct tasks instead of one + (N-1) ErrTaskIDConflicts
// (asynq TaskID uniqueness is global across the instance). A single-consumer event keeps a
// stable "<eventID>:<queue>" id — still unique per event, so the enqueue-dedup guarantee is
// preserved: a redelivered outbox row for the same event+queue still conflicts and is a no-op.
func taskIDForQueue(eventID, queue string) string {
	return eventID + ":" + queue
}

// ExtractTrace returns a ctx carrying the producer's span context, read from the
// traceparent header the relay stamped on the task. The consumer middleware uses it to
// LINK its span back to the producer (each event is its own trace; docs erd-backend
// §4c.3). With no header it returns ctx unchanged.
func ExtractTrace(ctx context.Context, t *asynq.Task) context.Context {
	return CtxWithTraceContext(ctx, t.Headers()[traceparentKey])
}

// queueFor routes a dotted event type to a work queue by its domain prefix so a
// slow queue (e.g. a 10-minute OCR in "documents") never blocks another kind of
// work (docs erd-backend §4c.2).
func queueFor(typ string) string {
	// diario_requested (national bulk ingestion) gets its OWN queue, consumed by a
	// dedicated concurrency-1 server: the DJEN diário fetch is slow and rate-limited by
	// a GLOBAL cumulative cap, so running it serialized (one stream) rather than 3-way
	// on "ingestao" both keeps it under the cap (concurrency made the 429s WORSE) and
	// stops it from starving — or being starved by — the enrichment/sync work.
	if typ == "acquisition.diario_requested" {
		return "diario"
	}
	// sync_completed/sync_failed (the backfill completion counter) get their OWN light
	// queue, drained by a dedicated concurrency-2 server: the increment/finalize is
	// trivial, but on "ingestao" it queues behind thousands of slow court_record_observed
	// enrichment tasks (~110s each, no preemption), so the backfill_job only flips to
	// COMPLETED once the enrichment drains. asynq has no per-task priority within a queue
	// (weight is per-queue), so a separate queue is the only way to let the sync finish
	// independently of the eventual DATAJUD enrichment. Must match worker-ingestao's
	// syncStatusQueue and the dedicated server's Queues below.
	if typ == "acquisition.sync_completed" || typ == "acquisition.sync_failed" {
		return "sync_status"
	}
	// enrichment_batch_requested (the self-re-enqueuing DATAJUD batch job) gets its OWN
	// queue, drained by a dedicated low-concurrency server: the batch is serialized by its
	// own DATAJUD rate limiter (a handful of _search requests), so it must not sit behind —
	// or compete on the same pool with — the sync work on "ingestao". It carries the
	// "acquisition" prefix, so it must be routed HERE, before the prefix switch sends
	// "acquisition" to "ingestao". String literal, not the slice's const (queueFor cannot
	// import internal/acquisition). Must match worker-ingestao's enrichmentQueue.
	if typ == "acquisition.enrichment_batch_requested" {
		return "enrichment"
	}
	// The deadline slice's async listener (intimation.observed/cancelled + the scheduled
	// reminder_check/missed_check self-messages) gets its OWN queue, drained by a dedicated
	// server on worker-ingestao: creating a prazo is fast (DB + outbox), but on "ingestao" it
	// queues behind the DATAJUD enrichment flood (thousands of ~110s court_record_observed
	// tasks, no per-task priority within a queue), so prazos come out starved (~16/min in
	// prod). A separate queue lets the prazo flow run independently — the same fix as
	// sync_status. intimation.observed/cancelled carry the "acquisition" prefix, so they must
	// be routed HERE (before the prefix switch sends "acquisition" to "ingestao"). String
	// literals, not the slice's consts: queueFor cannot import internal/deadline (import
	// cycle). Must match worker-ingestao's deadlineQueue and the dedicated server's Queues.
	if typ == "acquisition.intimation.observed" || typ == "acquisition.intimation.cancelled" ||
		typ == "deadline.reminder_check" || typ == "deadline.missed_check" ||
		typ == "deadline.opened" || typ == "acquisition.court_record_archived" {
		// deadline.opened is HIGH-VOLUME (one per prazo — a backfill mints thousands) and has
		// no functional consumer, but it MUST NOT ride "ingestao" (the orphan default in the
		// switch below): there it fails handler-not-found, retries, and STARVES the DATAJUD
		// enrichment sharing that queue. Route it to the deadline server, which acks it with a
		// no-op handler (deadline.Listener) — cheap, no retry, no queue clog.
		//
		// acquisition.court_record_archived (Achado 2, fatia 2b) joins them here for the SAME
		// starvation reason: OnCourtRecordArchived is fast (DB + outbox, no external fetch) and
		// has no OTHER consumer today (unlike docket_entry_observed's real fan-out), so a plain
		// single-queue route — not queuesFor's fan-out — is enough. It carries the "acquisition"
		// prefix, so it must be routed HERE too (before the prefix switch below sends
		// "acquisition" to "ingestao", where deadline.Listener has no mux mounted).
		return "deadline"
	}
	// actionitem.created/confirmed (fatia 3, docs/erd-costura-providencia-tarefa-peca.md §6):
	// deadline's listener reacts to either by creating a task right away — fast (DB + outbox),
	// the SAME starvation argument as the intimation events above, so it rides the SAME
	// dedicated "deadline" queue rather than "ingestao" (where the enrichment flood would
	// starve it) or "default" (the actionitem prefix's unrouted fallback, which no worker
	// drains). String literals, not internal/actionitem's consts: queueFor cannot import
	// internal/actionitem (it already avoids internal/deadline for the same reason above).
	// Must match worker-ingestao's deadlineQueue and the dedicated server's Queues.
	//
	// actionitem.reclassified (fatia 5, docs §7 questão 4) joins them here for the SAME
	// reason: internal/draft's reclassify listener (reclassify.go) just supersedes a draft
	// row — fast (DB only, no LLM) — so it must not queue behind the enrichment flood on
	// "ingestao" either. Mounted on the SAME dedicated "deadline" mux as deadline's own
	// actionitem.created/confirmed handlers, alongside internal/draft's ReclassifyListener.
	if typ == "actionitem.created" || typ == "actionitem.confirmed" || typ == "actionitem.reclassified" {
		return "deadline"
	}
	// deadline.due_soon/missed are NOT consumed by the deadline server — they are consumed by
	// the NOTIFICATIONS listener, which runs on the main server (it serves "notifications").
	// Route them to that queue so the reminder/miss avisos are delivered, not to "deadline"
	// (whose server has no notifications handler) nor to "ingestao". deadline.resolved_on_
	// conclusion (Achado 2, fatia 2c) joins them here for the identical reason — its only
	// consumer is the SAME notifications listener (a low-priority "resolvido" aviso).
	if typ == "deadline.due_soon" || typ == "deadline.missed" || typ == "deadline.resolved_on_conclusion" {
		return "notifications"
	}
	// The trial provisioning flow (fatia 2) shares the "notifications" queue too: billing
	// has no dedicated asynq server (it was webhook-only before this fatia), and the work is
	// light/low-volume (one provisioning + one scheduled check per tenant), so it rides the
	// main worker's mux alongside the notifications listener instead of standing up a new
	// dedicated server. identity.tenant_provisioned carries the "identity" prefix (would
	// otherwise fall to "default", which no worker drains — silently stalling every trial);
	// billing.trial_ending_soon_check/trial_ending_soon carry "billing" (also unrouted by
	// the prefix switch below). Must match worker-ingestao's billing.NewListener wiring.
	if typ == "identity.tenant_provisioned" || typ == "billing.trial_ending_soon_check" || typ == "billing.trial_ending_soon" {
		return "notifications"
	}
	// draft.generation_requested (Fatia 3) → "ai": consumed by worker-ai's
	// draft.Listener. String literal avoids an import cycle with internal/draft.
	if typ == "draft.generation_requested" {
		return "ai"
	}
	// court.fetch_autos_requested/fetch_autos_item_requested → "court_fetch": the
	// FetchAutosBatch self-re-enqueuing job + its per-item retries (internal/court),
	// consumed by cmd/worker-court's dedicated low-concurrency server (the real
	// ceiling is how many court_connection exist, not worker slots). String
	// literals, not the slice's consts — queueFor cannot import internal/court
	// (import cycle). court.connection_state_changed is DELIBERATELY left off this
	// list: it has no async consumer (the FE reads it via polling/SSE, not asynq),
	// so it stays on the prefix switch's "default" fallthrough below, same as
	// today — routing it here too would just move an already-orphaned event into a
	// queue with a real worker attached, turning silence into handler-not-found
	// noise for no benefit.
	if typ == "court.fetch_autos_requested" || typ == "court.fetch_autos_item_requested" {
		return "court_fetch"
	}
	// filing.enqueued → "filing": consumido pelo worker-filing (RPA e-SAJ).
	// String literal evita import cycle com internal/draft. filing.succeeded/
	// filing.failed são consumidos pelo listener de notificações (mesma fila
	// do resto das notificações).
	if typ == "filing.enqueued" {
		return "filing"
	}
	if typ == "filing.succeeded" || typ == "filing.failed" {
		return "notifications"
	}
	// draft.generated (GenerateUseCase.OnGenerationRequested, internal/draft/generate.go)
	// is consumed by acquisition's activity listener (process cockpit "Atividade"
	// timeline, migration 0073 — DRAFT_GENERATED). Light/low-volume (one per successful
	// Gerar call), so it rides the "notifications" queue alongside the other light
	// cross-slice consumers (identity.tenant_provisioned, billing.trial_*,
	// deadline.due_soon/missed) rather than standing up a dedicated queue. String
	// literal avoids an import cycle with internal/draft. Was "review.completed" until
	// 2026-08-26 — moved because Revisar never actually writes the peça's content (see
	// draft/events.go's note); draft.generated is the semantically correct producer.
	if typ == "draft.generated" {
		return "notifications"
	}
	switch prefix(typ) {
	case "ingestao", "acquisition":
		// The acquisition slice's events (integration_activated, sync_requested,
		// court_record_observed, …) are the ingestion work: worker-ingestao consumes
		// the "ingestao" queue, so route the acquisition domain there. Without this
		// they land in "default", which no worker consumes — the whole async
		// discovery/enrichment chain silently stalls.
		return "ingestao"
	case "deadline", "task":
		// The deadline slice's CONSUMED events (reminder_check/missed_check + high-volume
		// deadline.opened → "deadline"; due_soon/missed → "notifications"; actionitem.created/
		// confirmed → "deadline") are special-cased above. What reaches here via "deadline" is
		// the LOW-VOLUME rest — deadline.revoked/updated/met — with no async consumer today;
		// kept on "ingestao" (archived as handler-not-found), NOT "default", so the rows don't
		// pile up in a Redis queue no worker drains.
		//
		// "task" (task.created/updated/completed/dismissed) rides the SAME "ingestao" queue:
		// task.created is now a REAL consumer (fatia 3 — internal/actionitem's listener, mounted
		// on worker-ingestao's main mux, writes the reverse action_item.task_id pointer when the
		// event carries an action_item_id); the other three stay orphan-for-now (archived as
		// handler-not-found), same as the deadline.* rest above.
		return "ingestao"
	case "documents", "document":
		// The document slice PRODUCES document.* (singular prefix: document.uploaded,
		// .extracted, .ready, .failed — the ingest→extract→OCR→chunk→embed pipeline) and
		// worker-documents serves the "documents" queue where the extraction + indexing
		// listeners are mounted. Route BOTH the singular "document" and legacy plural
		// "documents" prefixes there — without the singular case they fall to "default",
		// which no worker consumes, and the whole pipeline silently stalls.
		return "documents"
	case "ai":
		return "ai"
	case "notification":
		return "notifications"
	default:
		return "default"
	}
}

// queuesFor is the FAN-OUT resolver: which asynq queues one event must be delivered to.
// Almost every event has a single consumer, so it returns queueFor(typ) as a 1-element
// slice — identical behavior, zero blast radius. The ONE exception is
// acquisition.docket_entry_observed, which has TWO independent consumers on two DIFFERENT
// asynq servers/muxes: notifications (main server, "ingestao"/"notifications" queues →
// new-andamento aviso) AND deadline (dedicated "deadline" server → reconcile a MISSED/OPEN
// prazo when a movimento de resposta lands). One outbox row → one asynq task → one queue →
// one mux → one handler, so a single delivery cannot reach both muxes; the relay instead
// enqueues ONE task per queue here. The current consumer ("ingestao") stays FIRST so the
// perceived priority is unchanged. Each consumer dedups under its OWN processed_event key
// (notifications' vs deadline.reconcile's), so at-least-once per consumer is expected and safe.
func queuesFor(typ string) []string {
	if typ == "acquisition.docket_entry_observed" {
		return []string{"ingestao", "deadline"}
	}
	// acquisition.court_record_observed gets a SECOND independent consumer the same
	// way: acquisition's own listener (DATAJUD enrichment trigger, "ingestao") is
	// joined by internal/court's FetchAutosBatch arrival trigger ("court_fetch",
	// dedicated low-concurrency server — real ceiling is how many court_connection
	// exist, not worker slots, see cmd/worker-court). "ingestao" stays FIRST so the
	// existing consumer's perceived priority is unchanged; each consumer dedups
	// under its own processed_event key.
	if typ == "acquisition.court_record_observed" {
		return []string{"ingestao", "court_fetch"}
	}
	return []string{queueFor(typ)}
}

// maxRetryFor sets the retry budget per work kind: sync tolerates flaky courts and
// retries generously; AI costs money per attempt, so it retries little (docs
// erd-backend §4c.4).
func maxRetryFor(typ string) int {
	switch prefix(typ) {
	case "ingestao", "acquisition":
		return 25
	case "documents", "document":
		return 10
	case "ai":
		return 3
	default:
		return 5
	}
}

// priorityFor sorts a dotted event type into the outbox drain priority written at
// Publish time (migration 0075): 0 (interactive) jumps ahead of 1 (background) in
// selectUnpublished's ORDER BY priority, id, so a mass backfill of bulk events (a
// DJEN sync minting ~10,000 acquisition.court_record_observed rows) can never
// starve an event a user is staring at a screen waiting for. This was a real
// production incident: draft.generation_requested sat behind the backfill for 8+
// minutes because the relay drained strict ORDER BY id with no priority.
//
// Only the EXACT types below are 0; everything else — including every unlisted or
// future event type — is 1 via the default branch. That is the fail-safe: a new
// event type nobody remembered to classify degrades to "background", never to
// silently jumping the queue.
//
// String literals, not another slice's consts: this file already documents (see
// queueFor) that lib/events cannot import internal/* without an import cycle.
func priorityFor(typ string) int16 {
	switch typ {
	case "draft.generation_requested",
		"draft.generated",
		"filing.enqueued",
		"filing.succeeded",
		"filing.failed",
		"identity.tenant_provisioned",
		"notification.requested",
		"billing.trial_ending_soon_check",
		"billing.trial_ending_soon",
		"billing.payment_failed":
		return 0
	default:
		return 1
	}
}

// prefix returns the segment before the first dot of a dotted event type, or the
// whole string when it has no dot.
func prefix(typ string) string {
	if i := strings.IndexByte(typ, '.'); i >= 0 {
		return typ[:i]
	}
	return typ
}
