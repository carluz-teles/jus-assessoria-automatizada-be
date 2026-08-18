// Command api is the HTTP entrypoint. It is the only binary that runs
// migrations, and the fully wired one: config → health → migrate → bootstrap →
// serve, with graceful shutdown draining in-flight requests (docs/erd-backend.md
// §5b.1, §1). The workers/relay/scheduler share the same boot lifecycle but are
// boot skeletons — their domain listeners land with later feature slices.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/internal/document"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/internal/lookup"
	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
	"github.com/jusassessoria/platform/lib/llm"
	"github.com/jusassessoria/platform/lib/pubsub"
	"github.com/jusassessoria/platform/lib/storage"
	"github.com/jusassessoria/platform/lib/telemetry"
	"github.com/jusassessoria/platform/pkg/lifecycle"
)

const serviceName = "api"

func main() {
	logger := telemetry.SetupDefault(os.Stdout, config.LogLevelFromEnv())
	if err := run(logger); err != nil {
		logger.Error("api boot failed", "error", err)
		os.Exit(1)
	}
}

// run performs the boot sequence and blocks in the serve loop until shutdown.
// Every boot step returns its error rather than calling Fatal itself (single
// handling rule) — main logs once and exits non-zero. It never returns until a
// signal drains the server, at which point it returns nil.
func run(logger *slog.Logger) error {
	ctx := context.Background()

	// 1. Config — a missing required var dies here, not mid-request.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// 2. Health — refuse to boot against unreachable dependencies.
	if err := health.WaitAll(ctx, cfg); err != nil {
		return fmt.Errorf("dependency health check: %w", err)
	}

	// 3. Migrations — the api, and only the api, applies the schema (§5b.3).
	// MIGRATE_RESET=true forces a one-time destructive reset (drop + re-apply) to
	// recover a database whose schema_migrations drifted from the real schema; unset
	// it right after so normal boots only apply pending migrations.
	if os.Getenv("MIGRATE_RESET") == "true" {
		logger.Warn("MIGRATE_RESET=true — resetting the schema before migrating")
		if err := database.Reset(ctx, cfg.DatabaseURL); err != nil {
			return fmt.Errorf("reset migrations: %w", err)
		}
	} else if err := database.Up(ctx, cfg.DatabaseURL); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}

	// 4. Bootstrap — telemetry first so every later component logs with trace
	// correlation and the shutdown hook can flush the providers.
	telemetryShutdown, err := telemetry.Setup(ctx, cfg, serviceName)
	if err != nil {
		return fmt.Errorf("setup telemetry: %w", err)
	}

	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open database pool: %w", err)
	}

	// Seed the national holiday calendar (BrasilAPI) for the current year plus
	// cfg.HolidaySeedYearsAhead, so lib/calendar can derive deadline dates. Fail-
	// soft: a BrasilAPI outage must never block boot — the seeder logs and
	// self-heals on the next boot once the provider recovers.
	if err := calendar.SeedNational(
		ctx, calendar.NewStore(pool), calendar.NewBrasilAPIFetcher(), logger,
		time.Now().UTC().Year(), cfg.HolidaySeedYearsAhead,
	); err != nil {
		logger.Warn("national holiday seed failed", "error", err)
	}

	redisOpt, err := asynq.ParseRedisURI(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis uri: %w", err)
	}
	asynqClient := asynq.NewClient(redisOpt)

	// A raw redis client (separate from the asynq client) backs the SSE stream's
	// subscribe side: the browser stream joins notif:<tenant> to receive the in-app
	// pushes the worker publishes (slice 2b). Same Redis, its own client, closed at
	// shutdown below.
	pubsubOpt, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse redis url for pub/sub: %w", err)
	}
	pubsubClient := redis.NewClient(pubsubOpt)

	// Identity wiring: the slice owns the domain; the binary only assembles it
	// (repo + shared outbox + unit of work → use case → resolver/webhook/handler).
	uow := database.NewUnitOfWork(pool)
	repo := identity.NewRepository(pool)
	// NewClerkVerifier calls clerk.SetKey first — NewClerkMembershipGateway relies
	// on that package-level key (it builds a zero-valued ClientConfig), so the
	// verifier must be constructed before the use case wires the gateway.
	verifier := middleware.NewClerkVerifier(cfg.ClerkSecret, cfg.ClerkIssuer)
	uc := identity.NewUseCase(repo, events.NewOutbox(), uow, identity.WithMembershipGateway(identity.NewClerkMembershipGateway()))
	resolver := identity.NewResolver(uc)
	webhook := identity.NewWebhookHandler(cfg.ClerkWebhookSecret, uc)
	identityHandler := identity.NewHandler(uc)

	// Acquisition wiring: the slice owns the domain; the binary only assembles it
	// (repo + shared outbox + unit of work → write use case; the same repo backs the
	// read use case for the processos/intimações screen reads). Activation is
	// entitlement-gated (the tenant's ACTIVE-process ceiling, read from billing), so an
	// over-limit onboarding is refused at the edge.
	acquisitionRepo := acquisition.NewRepository(pool)
	acquisitionEntitlement := resolveEntitlementChecker(cfg, billing.NewRepository(pool))

	// Deadline wiring: the slice's HTTP surface is the prazos screen reads (pool-backed)
	// PLUS the F2 confirmation write (POST /prazos/confirm, §9). The confirm runs on the
	// transactional path, so it needs the write use case's deps (sqlc repo + judicial
	// calendar + shared outbox + dedup + unit of work) — the same composition the worker's
	// creation listener uses. The event-driven creation path still lives in the worker.
	deadlineWriteUC := deadline.NewUseCase(
		deadline.NewRepository(),
		calendar.New(calendar.NewStore(pool)),
		events.NewOutbox(),
		deadline.NewDedup(),
		uow,
	)
	deadlineReadUC := deadline.NewReadUseCase(deadline.NewReadRepository(pool))
	// AI task suggestion (on-demand): the meta-prompt composer + the LLM generator (OpenRouter),
	// injected into a read-side use case. The generator is OPTIONAL — nil when OPENROUTER_API_KEY
	// is unset, so the endpoint returns no suggestions and the F2 form still works (no boot fail).
	var taskGenerator llm.Generator
	if cfg.OpenRouterAPIKey != "" {
		g, err := llm.NewOpenRouterGenerator(cfg.OpenRouterAPIKey, cfg.OpenRouterBaseURL, cfg.OpenRouterModel, nil)
		if err != nil {
			return fmt.Errorf("init openrouter generator: %w", err)
		}
		taskGenerator = g
	} else {
		logger.Warn("OPENROUTER_API_KEY unset — AI task suggestions disabled (F2 works, no pre-fill)")
	}
	// Process summary (AI) — GET /v1/processos/:id/resume. Same pattern as the deadline
	// suggester: the read use case + the shared meta-prompt composer + the SAME optional LLM
	// generator (same model — decision D of the handoff), so the api never builds a second
	// OpenRouter client. The Voyage embedder is OPTIONAL (best-effort RAG): when
	// VOYAGE_API_KEY is set it searches the process's document chunks to enrich the context;
	// otherwise (or on any fault) the summary is built without chunks — never a boot failure,
	// never a 5xx. The store write-throughs the ai_resume the FIRST time (WHERE ai_resume IS
	// NULL), so later opens serve the cache instead of asking the LLM again.
	var resumeEmbedder indexing.Embedder
	if cfg.VoyageAPIKey != "" {
		emb, err := indexing.NewVoyageEmbedder(cfg.VoyageAPIKey, cfg.VoyageModel, cfg.VoyageEmbedDim, nil)
		if err != nil {
			logger.Warn("voyage embedder init failed — RAG disabled for process summary", "error", err)
		} else {
			resumeEmbedder = emb
		}
	}
	resumoUC := acquisition.NewResumoUseCase(
		acquisition.NewReadUseCase(acquisitionRepo),
		advisory.NewTemplateComposer(),
		taskGenerator,
		acquisition.NewResumoStore(uow),
		resumeEmbedder,
		indexing.SearchDeps{Pool: pool},
		cfg.OpenRouterModel,
	)
	// Acquisition wiring: the slice owns the domain; the binary only assembles it
	// (repo + shared outbox + unit of work → write use case; the same repo backs the
	// read use case for the processos/intimações screen reads; the same read use case
	// + the resumoUC above back the AI process summary). Activation is
	// entitlement-gated (the tenant's ACTIVE-process ceiling, read from billing), so an
	// over-limit onboarding is refused at the edge.
	acquisitionHandler := acquisition.NewHandler(
		acquisition.NewUseCase(acquisitionRepo, events.NewOutbox(), uow,
			acquisition.WithActivationEntitlementChecker(acquisitionEntitlement)),
		acquisition.NewReadUseCase(acquisitionRepo),
		resumoUC,
	)
	// The suggester also captures provenance (feedback loop, camada 1): every suggestion batch
	// is persisted (best-effort) via its own tx so the confirm can later diff it against the
	// lawyer's confirmed tasks. The model string is recorded alongside the prompt_version. It
	// also write-throughs the "O que aconteceu"/"O que fazer" summary (migration 0036) the FIRST
	// time a prazo is summarized, so later opens serve the cache instead of asking the LLM again.
	deadlineSuggestUC := deadline.NewSuggestUseCase(
		deadlineReadUC, advisory.NewTemplateComposer(), taskGenerator,
		deadline.NewSuggestionStore(uow), deadline.NewAISummaryStore(uow), cfg.OpenRouterModel,
	)
	deadlineHandler := deadline.NewHandler(
		deadlineReadUC,
		deadlineWriteUC,
		deadlineSuggestUC,
	)

	// Billing wiring: the slice owns the domain; the binary only assembles it
	// (repo + Stripe gateway + shared outbox + dedup + unit of work + checkout
	// config → use case → webhook + endpoint handler). One use case backs both the
	// public webhook and the authenticated endpoints. The gateway is the sole holder
	// of the Stripe SDK and secrets.
	billingGateway := billing.NewStripeGateway(cfg.StripeSecretKey, cfg.StripeWebhookSecret)
	billingUC := billing.NewUseCase(
		billing.NewRepository(pool), billingGateway, events.NewOutbox(), billing.NewDedup(), uow,
		billing.WithCheckoutConfig(billing.CheckoutConfig{
			SuccessURL: cfg.BillingSuccessURL,
			CancelURL:  cfg.BillingCancelURL,
			ReturnURL:  cfg.BillingReturnURL,
			TrialDays:  cfg.StripeTrialDays,
		}),
		// The payment_failed e-mail fan-out (FLO-69) reads identity's own repo —
		// the same one already wired above for the identity use case — never
		// identity's use case/entity, so this stays a cross-slice PORT
		// (AdminLister), not an import of identity's domain.
		billing.WithAdminLister(identity.NewAdminListerAdapter(repo)),
	)
	billingWebhook := billing.NewWebhookHandler(billingUC)
	billingHandler := billing.NewHandler(billingUC)

	// Notifications wiring: the api mounts two surfaces off one repo — the public
	// provider (Resend) webhook (bounce/complaint callback, svix-verified, pool-only
	// use case: locate a delivery by provider id → flip status), and the authenticated
	// in-app inbox handler (slice 2a: list/badge/mark-read, read state per user). The
	// e-mail-sending listener lives in the worker, not here.
	notificationsRepo := notifications.NewRepository(pool)
	notificationsWebhook := notifications.NewWebhookHandler(
		notifications.NewSvixVerifier(cfg.ResendWebhookSecret),
		notifications.NewWebhookUseCase(notificationsRepo, uow),
	)
	notificationsHandler := notifications.NewHandler(
		notifications.NewReadUseCase(notificationsRepo, uow),
		pubsub.NewRedisPubSub(pubsubClient),
		notifications.NewPreferenceUseCase(notificationsRepo, uow),
	)

	// Lookup wiring: a stateless proxy over the BrasilAPI registry. No pool, no
	// outbox — the slice owns only an HTTP port; the binary just injects the
	// client and mounts the handler.
	lookupHandler := lookup.NewHandler(lookup.NewBrasilAPIClient())

	// Storage is optional at v0: only wired when S3 is fully configured. The Documentos
	// slice (Fatia 1) consumes it — the presigned upload/download — so when storage is
	// present the document write use case + handler are built off it (repo + storage +
	// shared outbox + unit of work), mirroring the deadline composition. Built to fail
	// fast on bad credentials at boot and logged as ready. Without S3 the slice stays
	// unmounted (documentHandler nil → the router nil-guard skips it).
	var documentHandler *document.Handler
	if cfg.S3Enabled() {
		storageClient, err := storage.New(ctx, storage.Options{
			Endpoint:  cfg.S3Endpoint,
			Region:    cfg.S3Region,
			Bucket:    cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
		})
		if err != nil {
			return fmt.Errorf("init storage: %w", err)
		}
		logger.Info("storage configured", "bucket", cfg.S3Bucket)

		documentWriteUC := document.NewUseCase(
			document.NewRepository(),
			storageClient,
			events.NewOutbox(),
			uow,
		)
		documentHandler = document.NewHandler(
			document.NewReadUseCase(document.NewReadRepository(pool)),
			documentWriteUC,
		)
	}

	// Draft (Peticionamento Fatias 1–3b): the peça create/read/autosave surface +
	// the AI generation trigger (Fatia 3) + grounded chat (Fatia 3b). Pool-backed
	// read (GetDetail) + transactional writes via the shared UoW. No storage required
	// (content lives in the DB column, not S3).
	// The trigger use case (Fatia 3) flips saga→EXTRACTING and publishes
	// draft.generation_requested into the outbox; worker-ai does the async generation.
	// The chat use case (Fatia 3b) is synchronous in-process (same request/response
	// cycle) — no outbox needed. It reuses the SAME taskGenerator + resumeEmbedder
	// already wired above (same OpenRouter model slug, same Voyage embedder).
	draftRepo := draft.NewRepository()
	draftUC := draft.NewUseCase(uow, draftRepo)
	draftTrigger := draft.NewTriggerUseCase(uow, draftRepo, events.NewOutbox())
	draftHandler := draft.NewHandler(draftUC).WithGenerator(draftTrigger)

	// Chat use case (Fatia 3b): reuses taskGenerator + resumeEmbedder + advisory
	// composer already instantiated above. nil generator / nil embedder → degraded
	// (ErrIANotConfigured / grounded=false) — never a boot failure.
	var chatEmbedder draft.VoyageEmbedder
	if resumeEmbedder != nil {
		chatEmbedder = resumeEmbedder
	}
	chatUC := draft.NewChatUseCase(draft.ChatUseCaseParams{
		UoW:      uow,
		Repo:     draftRepo,
		Gen:      taskGenerator,
		Emb:      chatEmbedder,
		Search:   indexing.SearchDeps{Pool: pool},
		Composer: advisory.NewTemplateComposer(),
		Model:    cfg.OpenRouterModel,
	})
	draftHandler = draftHandler.WithChat(chatUC)

	// 5. Router — the testable seam; no I/O happens here.
	app := newRouter(routerDeps{
		logger:               logger,
		corsOrigins:          cfg.CORSAllowedOrigins,
		verifier:             verifier,
		resolver:             resolver,
		webhook:              webhook,
		billingWebhook:       billingWebhook,
		notificationsWebhook: notificationsWebhook,
		notifications:        notificationsHandler,
		billing:              billingHandler,
		identity:             identityHandler,
		acquisition:          acquisitionHandler,
		deadline:             deadlineHandler,
		document:             documentHandler,
		draft:                draftHandler,
		lookup:               lookupHandler,
	})

	// 6. Serve with graceful shutdown. Listen blocks until ShutdownWithContext
	// returns it; the shutdown hook drains HTTP first, then releases the pool,
	// asynq client and telemetry providers.
	lifecycle.RunWithGracefulShutdown(
		serviceName,
		func() error {
			if err := app.Listen(":" + cfg.Port); err != nil {
				return fmt.Errorf("http listen: %w", err)
			}
			return nil
		},
		func(shutdownCtx context.Context) error {
			var errs []error
			if err := app.ShutdownWithContext(shutdownCtx); err != nil {
				errs = append(errs, fmt.Errorf("shutdown http: %w", err))
			}
			pool.Close()
			if err := asynqClient.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close asynq client: %w", err))
			}
			if err := pubsubClient.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close pub/sub redis client: %w", err))
			}
			if err := telemetryShutdown(shutdownCtx); err != nil {
				errs = append(errs, fmt.Errorf("shutdown telemetry: %w", err))
			}
			return errors.Join(errs...)
		},
	)

	return nil
}

// resolveEntitlementChecker decides which EntitlementChecker acquisition's
// activation gate uses. TEMPORARY: while config.BillingGateEnabled is false
// (the default), plan pricing is not yet decided (pending business decision),
// so it wires acquisition.NewUnlimitedEntitlementChecker() instead of the real
// billing.EntitlementAdapter — no tenant is blocked. The enforcement logic in
// billing.EntitlementAdapter is untouched; only this composition decides
// whether it is consulted. Flip BILLING_GATE_ENABLED=true (or remove the flag
// entirely, if it stops making sense) once pricing lands.
func resolveEntitlementChecker(cfg config.Config, repo billing.Repository) acquisition.EntitlementChecker {
	slog.Info("billing gate", "enabled", cfg.BillingGateEnabled)
	if !cfg.BillingGateEnabled {
		return acquisition.NewUnlimitedEntitlementChecker()
	}
	return billing.NewEntitlementAdapter(repo)
}
