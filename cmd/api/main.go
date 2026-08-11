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
	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/internal/lookup"
	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/config"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
	"github.com/jusassessoria/platform/lib/health"
	"github.com/jusassessoria/platform/lib/httpx/middleware"
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
	uc := identity.NewUseCase(repo, events.NewOutbox(), uow)
	verifier := middleware.NewClerkVerifier(cfg.ClerkSecret, cfg.ClerkIssuer)
	resolver := identity.NewResolver(uc)
	webhook := identity.NewWebhookHandler(cfg.ClerkWebhookSecret, uc)
	identityHandler := identity.NewHandler(uc)

	// Acquisition wiring: the slice owns the domain; the binary only assembles it
	// (repo + shared outbox + unit of work → write use case; the same repo backs the
	// read use case for the processos/intimações screen reads). The billing
	// entitlement adapter (a pool read, independent of billingUC below) is injected
	// so ActivateIntegration gates AT THE EDGE — the ERD's "entitlements na borda":
	// a tenant already at its active_process_limit is refused before it ever waits
	// on a backfill the worker's own gate (cmd/worker-ingestao) would mostly discard.
	acquisitionRepo := acquisition.NewRepository(pool)
	acquisitionEntitlement := billing.NewEntitlementAdapter(billing.NewRepository(pool))
	acquisitionHandler := acquisition.NewHandler(
		acquisition.NewUseCase(acquisitionRepo, events.NewOutbox(), uow,
			acquisition.WithActivationEntitlementChecker(acquisitionEntitlement)),
		acquisition.NewReadUseCase(acquisitionRepo),
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
	)

	// Lookup wiring: a stateless proxy over the BrasilAPI registry. No pool, no
	// outbox — the slice owns only an HTTP port; the binary just injects the
	// client and mounts the handler.
	lookupHandler := lookup.NewHandler(lookup.NewBrasilAPIClient())

	// Storage is optional at v0: only wired when S3 is fully configured. No route
	// consumes it yet — the upload slice injects it — so it is built to fail fast
	// on bad credentials at boot and logged as ready.
	if cfg.S3Enabled() {
		if _, err := storage.New(ctx, storage.Options{
			Endpoint:  cfg.S3Endpoint,
			Region:    cfg.S3Region,
			Bucket:    cfg.S3Bucket,
			AccessKey: cfg.S3AccessKey,
			SecretKey: cfg.S3SecretKey,
		}); err != nil {
			return fmt.Errorf("init storage: %w", err)
		}
		logger.Info("storage configured", "bucket", cfg.S3Bucket)
	}

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
