// Command worker-ingestao consumes the "ingestao" asynq queue (court/process
// sync jobs). This is a boot skeleton: it stands up the same lifecycle as the
// api — config → health → bootstrap → serve — registers the queue, and shuts
// down gracefully. The task handlers are registered here by the acquisition
// feature slice; the mux is intentionally empty for now. No migrations: workers
// assume the api already applied the schema (§5b.3).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/pubsub"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const (
	serviceName = "worker-ingestao"
	queueName   = "ingestao"
	// notificationsQueue carries the avisos domain's `notification.*` events. It
	// shares this worker (the process where the async listeners live) but its own
	// queue, so a slow e-mail send never blocks court sync.
	notificationsQueue = "notifications"
	// diarioQueue carries the national bulk ingestion (diario_requested). It is drained
	// by a SEPARATE asynq server at diarioConcurrency=1 so the slow, globally-rate-limited
	// DJEN diário fetch runs serialized — concurrency made the 429s WORSE (it trips the
	// endpoint's cumulative cap faster) — and never competes with the enrichment/sync work
	// on the "ingestao" queue. Must match lib/events.queueFor's routing of the event.
	diarioQueue = "diario"
	// diarioConcurrency is fixed at 1: the whole point is to serialize the national fetch
	// against the DJEN global cap. Not env-tunable — a higher value reintroduces the storm.
	diarioConcurrency = 1
	// syncStatusQueue carries the backfill completion counter (sync_completed/sync_failed).
	// It is drained by a SEPARATE asynq server so the light increment/finalize that flips
	// backfill_job to COMPLETED never queues behind the enrichment flood (thousands of slow
	// court_record_observed tasks) on "ingestao" — asynq has no per-task priority within a
	// queue. Must match lib/events.queueFor's routing of these two events.
	syncStatusQueue = "sync_status"
	// syncStatusConcurrency is fixed at 2: advancing the counter and finalizing the job is
	// trivial work, so 2 slots are plenty and, on their own queue, never compete with (or
	// wait behind) the ~110s enrichment fetches on "ingestao".
	syncStatusConcurrency = 2
	// deadlineQueue carries the prazo flow (acquisition.intimation.observed/cancelled and the
	// scheduled deadline.reminder_check/missed_check). It is drained by a SEPARATE asynq server
	// so creating a prazo — fast DB+outbox work — is never stuck behind the DATAJUD enrichment
	// flood on "ingestao" (thousands of ~110s court_record_observed tasks, no per-task priority
	// within a queue), which in prod starved prazos to ~16/min. Same starvation fix as
	// syncStatusQueue. Must match lib/events.queueFor's routing of these events.
	deadlineQueue = "deadline"
	// enrichmentQueue carries the batch DATAJUD enrichment job (enrichment_batch_requested).
	// It is drained by a SEPARATE low-concurrency server: the batch is serialized by the
	// DATAJUD connector's own rate limiter (a handful of _search requests per import), so it
	// must not compete on the "ingestao" pool with the sync work. Must match
	// lib/events.queueFor's routing of the event.
	enrichmentQueue = "enrichment"
	// enrichmentConcurrency is fixed at 2: the DATAJUD limiter (shared connector) already
	// serializes the requests, so a couple of slots cover the self-re-enqueue overlap
	// between one import's tribunals without hammering the API. Not env-tunable — the true
	// throttle is the limiter, not the slot count.
	enrichmentConcurrency = 2
	// deadlineConcurrency is fixed at 8: deriving a prazo is a short DB+outbox transaction
	// (no slow external fetch, unlike the enrichment), so a handful of slots clears the
	// intimation backlog quickly; on its own queue those 8 workers never compete with the
	// enrichment/sync work on "ingestao". Not env-tunable — the work is cheap and bounded.
	deadlineConcurrency = 8
	// defaultConcurrency backs INGESTAO_CONCURRENCY when unset/invalid. Parallelism
	// is the backfill's main lever: DJEN/DATAJUD fetches are LONG (~110s each, stuck
	// on the residential proxy), so running many at once overlaps that wait and cuts
	// the wall clock ~N×. The concurrent writes this unlocks are serialized per tenant
	// by the AcquireTenantWriteLock advisory lock, so there is no 40P01 deadlock.
	defaultConcurrency = 8
	// onboardingHistoryDays is how far back a new tenant is caught up from the stored
	// firehose on the cutover. Aligned to the lean-ingestion 30-day horizon (the same
	// window the per-OAB backfill now reaches): without this, the cutover would still
	// sweep a full year of the firehose even though the backfill only spans 30 days.
	onboardingHistoryDays = 30
)

func main() {
	logger := telemetry.SetupDefault(os.Stdout, config.LogLevelFromEnv())
	if err := run(logger); err != nil {
		logger.Error("worker boot failed", "service", serviceName, "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := health.WaitAll(ctx, cfg); err != nil {
		return fmt.Errorf("dependency health check: %w", err)
	}

	telemetryShutdown, err := telemetry.Setup(ctx, cfg, serviceName)
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}

	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}

	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis uri: %w", err)
	}

	concurrency := cfg.IngestaoConcurrency
	if concurrency <= 0 {
		concurrency = defaultConcurrency
	}
	logger.Info("worker concurrency", "service", serviceName, "concurrency", concurrency)

	// asynq's own logs go through slog (structured, OTLP); LogLevel=ErrorLevel keeps
	// its per-task retry Warns from duplicating the Observe middleware's failure log.
	// The MAIN server drains the enrichment/sync ("ingestao") and avisos
	// ("notifications") queues; the national bulk ingestion runs on its own server below.
	srv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: concurrency,
		Queues: map[string]int{
			queueName:          concurrency,
			notificationsQueue: concurrency,
		},
		Logger:   events.NewAsynqLogger(logger),
		LogLevel: asynq.ErrorLevel,
	})

	// Feature slices register their listeners on this mux. Each slice owns its
	// task-type registration via a Register(mux) call; the worker only composes.
	// Observe wraps EVERY handler: consumer span + failure log, one place.
	mux := asynq.NewServeMux()
	mux.Use(events.Observe(logger))

	repo := acquisition.NewRepository(pool)
	outbox := events.NewOutbox()
	uow := database.NewUnitOfWork(pool)

	// The sync use case resolves its connector per event from the orchestrator, by
	// the event's source. Both connectors are REAL now: DJEN discovers nationally by
	// OAB (no auth); DATAJUD enriches one process by number from the tribunal's index
	// (public API key). DATAJUD never discovers, so it is not an activatable
	// integration — it runs only through the enrichment listener below.
	orchestrator := acquisition.NewOrchestrator()

	// DJEN's Comunica WAF 403s the datacenter egress IP; when DJEN_PROXY_URL is set,
	// route the connector through a residential/BR proxy for a clean IP. Unset = a
	// direct connection (dev local passes without it). A malformed URL fails the boot.
	djenOpts := []acquisition.DJENOption{}
	if cfg.DJENProxyURL != "" {
		proxyURL, perr := url.Parse(cfg.DJENProxyURL)
		if perr != nil {
			return fmt.Errorf("parse DJEN_PROXY_URL: %w", perr)
		}
		djenOpts = append(djenOpts, acquisition.WithDJENProxy(proxyURL))
		logger.Info("DJEN outbound proxy enabled", "service", serviceName, "proxy_host", proxyURL.Host)
	}
	// A gentler pace than the connector's 1 req/s default, dialable per env when a big
	// OAB sweep 429s the DJEN egress (0 = keep the default).
	if cfg.DJENRatePerMinute > 0 {
		djenOpts = append(djenOpts, acquisition.WithDJENRatePerMinute(cfg.DJENRatePerMinute))
		logger.Info("DJEN rate override", "service", serviceName, "rate_per_minute", cfg.DJENRatePerMinute)
	}
	// Page size override (0 = keep the connector default of 1000). A bigger page means
	// fewer requests per window against the 1 req/s pace.
	if cfg.DJENPageSize > 0 {
		djenOpts = append(djenOpts, acquisition.WithDJENPageSize(cfg.DJENPageSize))
		logger.Info("DJEN page size override", "service", serviceName, "page_size", cfg.DJENPageSize)
	}
	// One DJEN connector instance backs both the per-OAB sync (via the orchestrator) and
	// the national ingestion consumer (FetchDiario), so they share the same egress: proxy,
	// rate limiter and page size. The national fetch is now the worker's job (it moved off
	// the scheduler in the event-driven ingestion redesign).
	djenConnector := acquisition.NewDJENConnector(djenOpts...)
	orchestrator.Register(acquisition.SourceDJEN, djenConnector)
	// One DATAJUD connector instance backs BOTH the per-record enrichment (via the
	// orchestrator, FETCH_BY_NUMBER) and the batch enrichment job (FETCH_BATCH, injected
	// directly below), so they share one egress + one rate limiter — the limiter is what
	// keeps the batch under the DATAJUD request-per-minute ceiling.
	datajudConnector := acquisition.NewDATAJUDConnector()
	orchestrator.Register(acquisition.SourceDATAJUD, datajudConnector)

	// Parsers are resolved by CanParse: the DJEN parser claims DJEN payloads and
	// derives the CPC-224 publication/deadline dates through the judicial calendar
	// (holiday table); the DATAJUD parser claims DATAJUD payloads and maps the graded
	// process + its movimentos.
	cal := calendar.New(calendar.NewStore(pool))
	parser := acquisition.ParserSet{
		acquisition.NewDJENParser(cal),
		acquisition.NewDATAJUDParser(),
	}

	// Backfill onboarding, built after the parser so the cutover can wire the match.
	// The window is dialed from BACKFILL_WINDOW_DAYS. When INGESTION_ENABLED, the
	// cutover swaps the per-OAB backfill for a history catch-up against the stored
	// firehose (MatchTenantSince over the retention window) — a new tenant sees its
	// recent intimações immediately with zero per-OAB DJEN calls, and the scheduler's
	// daily match covers everything forward. Off = the legacy per-OAB backfill.
	// ingestion is the national bulk consumer (diario_requested → FetchDiario + land),
	// built only when the pivot is on; nil keeps the listener inert to the event.
	var ingestion *acquisition.IngestionUseCase
	var backfill *acquisition.BackfillUseCase
	if cfg.IngestionEnabled {
		ingestion = acquisition.NewIngestionUseCase(djenConnector, repo, uow)
		match := acquisition.NewMatchUseCase(repo, uow, parser)
		backfill = acquisition.NewBackfillUseCase(repo, outbox, uow,
			acquisition.WithBackfillWindowDays(cfg.BackfillWindowDays),
			acquisition.WithHistoryMatcher(match, onboardingHistoryDays),
		)
		logger.Info("onboarding cutover enabled (history match, no per-OAB backfill)",
			"service", serviceName, "history_days", onboardingHistoryDays)
	} else {
		backfill = acquisition.NewBackfillUseCase(repo, outbox, uow,
			acquisition.WithBackfillWindowDays(cfg.BackfillWindowDays),
		)
	}
	if cfg.BackfillWindowDays > 0 {
		logger.Info("backfill window override", "service", serviceName, "window_days", cfg.BackfillWindowDays)
	}

	// The sync use case is entitlement-gated: a window that would take the tenant over its
	// ACTIVE-process ceiling (from billing) is refused, so the per-OAB backfill honors the
	// plan limit instead of importing unlimited processes.
	entitlement := resolveEntitlementChecker(cfg, billing.NewRepository(pool))
	sync := acquisition.NewSyncUseCase(repo, outbox, uow, orchestrator, parser,
		acquisition.WithEntitlementChecker(entitlement))

	// DATAJUD enrichment reacts to court_record_observed (a DJEN placeholder,
	// degree=UNKNOWN): it fetches the process by number to reveal the grau and does
	// the placeholder+merge. It shares the orchestrator and the ParserSet.
	enrichment := acquisition.NewEnrichmentUseCase(repo, outbox, uow, orchestrator, parser)

	// The batch enrichment job reacts to enrichment_batch_requested (emitted per tribunal on
	// backfill discovery): it scans a tribunal's due records, pulls them in ONE DATAJUD
	// _search (terms), grades all, and OWNS the import's ENRICHMENT capture row (opens →
	// progresses → closes deterministically when the scan drains). It reuses the enrichment
	// use case's shared grade core (gradeInTx) and the DATAJUD connector/parser's batch
	// methods (FetchBatch/ParseBatch). It runs on the dedicated "enrichment" server below.
	enrichmentBatch := acquisition.NewEnrichmentBatchUseCase(repo, enrichment, datajudConnector, acquisition.NewDATAJUDParser(), outbox, uow)

	// The listener mounts the shared enrichment/sync/backfill handlers on the MAIN mux.
	// diario_requested and enrichment_batch_requested are NOT among them — they run on the
	// dedicated servers below. Register never dereferences the ingestion dep, so a nil (pivot
	// off) is harmless here.
	listener := acquisition.NewListener(backfill, sync, enrichment, enrichmentBatch, ingestion)
	listener.Register(mux)

	// notifications: consume notification.requested and deliver by e-mail (Resend).
	// The provider config is required here (this worker runs the listener), so a
	// missing key fails the boot rather than silently dropping avisos later.
	resendClient, err := notifications.NewResendClient(cfg.ResendAPIKey)
	if err != nil {
		return fmt.Errorf("build resend client: %w", err)
	}
	emailChannel, err := notifications.NewEmailChannel(cfg.ResendFromEmail, resendClient)
	if err != nil {
		return fmt.Errorf("build email channel: %w", err)
	}
	// The two notifications use cases share a pool-backed repo and the stateless dedup:
	// the email consumer (notification.requested) and the in-app consumer (slice 1a),
	// which turns acquisition's backfill_finished/docket_entry_observed into IN_APP
	// avisos. Registering the in-app handlers here is what lets acquisition drop its
	// drainUnconsumed placeholder for those two types (one handler per type on the mux).
	// A dedicated redis client for the best-effort in-app push (slice 2b): when the
	// in-app consumer persists an aviso it publishes it on notif:<tenant> for the SSE
	// endpoint (a later slice) to relay in real time. It shares the same Redis as asynq
	// but its own client, closed in the graceful shutdown below.
	redisClientOpt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url for pub/sub: %w", err)
	}
	pubsubClient := redis.NewClient(redisClientOpt)

	notifRepo := notifications.NewRepository(pool)
	notifDedup := notifications.NewDedup()
	notifyUC := notifications.NewNotifyUseCase(notifRepo, emailChannel, notifDedup, uow)
	inAppUC := notifications.NewInAppUseCase(notifRepo, notifDedup, uow, pubsub.NewRedisPubSub(pubsubClient))
	notifications.NewListener(notifyUC, inAppUC).Register(mux)

	// acquisition's activity listener (process cockpit "Atividade" timeline, migration
	// 0073): consumes draft's draft.generated (a successful Gerar call) and appends a
	// DRAFT_GENERATED row. Rides the SAME "notifications" queue/mux as the listener
	// above (see lib/events' queueFor routing) — light, low-volume, one per Gerar
	// call. repo already satisfies activityCourtRecordResolver (ResolveCourtRecordIDForDraftIntimation).
	activityUC := acquisition.NewActivityUseCase(repo, acquisition.NewActivityDeduper(), acquisition.NewActivityLogWriter(), uow)
	acquisition.NewActivityListener(activityUC).Register(mux)

	// billing (fatia 2): consume identity.tenant_provisioned (start the tenant's trial)
	// and the scheduled billing.trial_ending_soon_check (re-check + warn). This is the
	// FIRST asynq consumer billing needs (it was webhook-only until now); both types
	// route to "notifications" (see lib/events' queueFor) so they ride the main mux
	// rather than a new dedicated server — the work is light and low-volume. The
	// gateway is nil: neither handler ever calls Stripe (that is the webhook path,
	// wired separately in cmd/api).
	billingUC := billing.NewUseCase(billing.NewRepository(pool), nil, outbox, billing.NewDedup(), uow)
	billing.NewListener(billingUC).Register(mux)

	// deadline (slice 2c): consume acquisition.intimation.observed and derive the prazo
	// deterministically (rules layer → the shared judicial calendar `cal`), persisting it
	// PENDING and emitting deadline.opened in one idempotent tx. The use case is built here,
	// but its listener mounts on the DEDICATED deadline server below (not this main mux): the
	// prazo flow moved off "ingestao" so it stops being starved by the enrichment flood.
	deadlineUC := deadline.NewUseCase(deadline.NewRepository(), cal, outbox, deadline.NewDedup(), uow)

	if err := srv.Start(mux); err != nil {
		return fmt.Errorf("start asynq server: %w", err)
	}

	// The DEDICATED backfill completion server: one small pool, its own "sync_status" queue,
	// so the light sync_completed/sync_failed counter that flips backfill_job to COMPLETED is
	// never stuck behind the enrichment flood on "ingestao". Built ALWAYS (unlike diarioSrv):
	// every backfill must be able to finalize, pivot on or off. sync_status is deliberately
	// NOT in the main srv's Queues, or it would compete on that pool again.
	syncStatusSrv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: syncStatusConcurrency,
		Queues:      map[string]int{syncStatusQueue: syncStatusConcurrency},
		Logger:      events.NewAsynqLogger(logger),
		LogLevel:    asynq.ErrorLevel,
	})
	syncStatusMux := asynq.NewServeMux()
	syncStatusMux.Use(events.Observe(logger))
	listener.RegisterSyncStatus(syncStatusMux)
	if err := syncStatusSrv.Start(syncStatusMux); err != nil {
		return fmt.Errorf("start sync_status asynq server: %w", err)
	}
	logger.Info("backfill completion server started (dedicated)",
		"service", serviceName, "queue", syncStatusQueue, "concurrency", syncStatusConcurrency)

	// The DEDICATED deadline server: its own "deadline" queue, so the fast prazo flow
	// (intimation.observed/cancelled + the scheduled reminder_check/missed_check) is never
	// stuck behind the enrichment flood on "ingestao" — the same starvation fix as the
	// sync_status server above. Built ALWAYS: every intimation must be able to open its prazo.
	// deadline is deliberately NOT in the main srv's Queues, or it would compete on that pool
	// again. deadline.due_soon/missed stay on the MAIN server (queue "notifications", the
	// notifications listener) — they are not consumed here.
	deadlineSrv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: deadlineConcurrency,
		Queues:      map[string]int{deadlineQueue: deadlineConcurrency},
		Logger:      events.NewAsynqLogger(logger),
		LogLevel:    asynq.ErrorLevel,
	})
	deadlineMux := asynq.NewServeMux()
	deadlineMux.Use(events.Observe(logger))
	deadline.NewListener(deadlineUC).Register(deadlineMux)
	if err := deadlineSrv.Start(deadlineMux); err != nil {
		return fmt.Errorf("start deadline asynq server: %w", err)
	}
	logger.Info("deadline server started (dedicated)",
		"service", serviceName, "queue", deadlineQueue, "concurrency", deadlineConcurrency)

	// The DEDICATED enrichment server: its own "enrichment" queue, so the batch DATAJUD job
	// (serialized by the connector's rate limiter) is isolated from the sync work on
	// "ingestao" — it neither starves nor is starved by it. Built ALWAYS: every backfill's
	// 2nd phase (enrichment) must be able to run and close its capture row. enrichment is
	// deliberately NOT in the main srv's Queues, or it would compete on that pool again.
	enrichmentSrv := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: enrichmentConcurrency,
		Queues:      map[string]int{enrichmentQueue: enrichmentConcurrency},
		Logger:      events.NewAsynqLogger(logger),
		LogLevel:    asynq.ErrorLevel,
	})
	enrichmentMux := asynq.NewServeMux()
	enrichmentMux.Use(events.Observe(logger))
	listener.RegisterEnrichment(enrichmentMux)
	if err := enrichmentSrv.Start(enrichmentMux); err != nil {
		return fmt.Errorf("start enrichment asynq server: %w", err)
	}
	logger.Info("enrichment server started (dedicated)",
		"service", serviceName, "queue", enrichmentQueue, "concurrency", enrichmentConcurrency)

	// The DEDICATED national-ingestion server: one worker, its own "diario" queue, so the
	// slow globally-rate-limited DJEN diário fetch runs serialized (never 3-way hammering
	// the cumulative cap) and stays off the enrichment/sync queue. Built only when the
	// pivot is on; its own mux carries just the diario_requested handler.
	var diarioSrv *asynq.Server
	if ingestion != nil {
		diarioSrv = asynq.NewServer(redisOpt, asynq.Config{
			Concurrency: diarioConcurrency,
			Queues:      map[string]int{diarioQueue: diarioConcurrency},
			Logger:      events.NewAsynqLogger(logger),
			LogLevel:    asynq.ErrorLevel,
		})
		diarioMux := asynq.NewServeMux()
		diarioMux.Use(events.Observe(logger))
		listener.RegisterIngestion(diarioMux)
		if err := diarioSrv.Start(diarioMux); err != nil {
			return fmt.Errorf("start diario asynq server: %w", err)
		}
		logger.Info("national ingestion server started (dedicated, serialized)",
			"service", serviceName, "queue", diarioQueue, "concurrency", diarioConcurrency)
	}

	// srv.Start is non-blocking; block serve until the shutdown hook stops the
	// server, so the lifecycle owns the single signal handler (§5b.1).
	stopped := make(chan struct{})
	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			<-stopped
			return nil
		},
		func(shutdownCtx context.Context) error {
			srv.Shutdown() // drains in-flight tasks before returning
			syncStatusSrv.Shutdown()
			deadlineSrv.Shutdown()
			enrichmentSrv.Shutdown()
			if diarioSrv != nil {
				diarioSrv.Shutdown()
			}
			close(stopped)
			if err := pubsubClient.Close(); err != nil {
				logger.Warn("close pub/sub redis client", "service", serviceName, "error", err)
			}
			pool.Close()
			if err := telemetryShutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shutdown telemetry: %w", err)
			}
			return nil
		},
	)

	return nil
}

// resolveEntitlementChecker decides which EntitlementChecker the sync cycle
// uses. TEMPORARY: while config.BillingGateEnabled is false (the default),
// plan pricing is not yet decided (pending business decision), so it wires
// acquisition.NewUnlimitedEntitlementChecker() instead of the real
// billing.EntitlementAdapter — no tenant's backfill/sync is refused for being
// over a process ceiling. The enforcement logic in billing.EntitlementAdapter
// is untouched; only this composition decides whether it is consulted. Flip
// BILLING_GATE_ENABLED=true (or remove the flag entirely, if it stops making
// sense) once pricing lands.
func resolveEntitlementChecker(cfg config.Config, repo billing.Repository) acquisition.EntitlementChecker {
	slog.Info("billing gate", "service", serviceName, "enabled", cfg.BillingGateEnabled)
	if !cfg.BillingGateEnabled {
		return acquisition.NewUnlimitedEntitlementChecker()
	}
	return billing.NewEntitlementAdapter(repo)
}
