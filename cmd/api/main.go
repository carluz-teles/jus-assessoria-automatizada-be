// Command api is the HTTP entrypoint. It is the only binary that runs
// migrations, and the fully wired one: config → health → migrate → bootstrap →
// serve, with graceful shutdown draining in-flight requests (docs/erd-backend.md
// §5b.1, §1). The workers/relay/scheduler share the same boot lifecycle but are
// boot skeletons — their domain listeners land with later feature slices.
package main

import (
	"context"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/internal/aiusage"
	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/internal/certificate"
	"github.com/jusassessoria/platform/internal/court"
	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/internal/document"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/internal/indexing"
	"github.com/jusassessoria/platform/internal/lookup"
	"github.com/jusassessoria/platform/internal/notifications"
	"github.com/jusassessoria/platform/internal/onboarding"
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
	"github.com/jusassessoria/platform/lib/vault"
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
		// The read-only confirmation-panel preview (POST /v1/prazos/preview) reads the intimação's
		// anchors/court on the pool, off the transactional path.
		deadline.WithPreviewPool(pool),
	)
	deadlineReadUC := deadline.NewReadUseCase(deadline.NewReadRepository(pool))
	// AI task suggestion (on-demand): the meta-prompt composer + the LLM generator (OpenRouter),
	// injected into a read-side use case. The generator is OPTIONAL — nil when OPENROUTER_API_KEY
	// is unset, so the endpoint returns no suggestions and the F2 form still works (no boot fail).
	var taskGenerator llm.Generator
	if cfg.OpenRouterAPIKey != "" {
		g, err := llm.NewOpenRouterGenerator(cfg.OpenRouterAPIKey, cfg.OpenRouterBaseURL, cfg.OpenRouterModel, nil, aiusage.NewRecorder(pool))
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
		// QUALITY: resumo executivo do processo é referência decisória — precisa fidelidade.
		cfg.OpenRouterModelQuality,
	)
	// Intimation analysis (AI) — POST /v1/intimacoes/:id/analise ("Analisar esta intimação").
	// Same pattern as resumoUC: the read use case + the shared meta-prompt composer + the SAME
	// optional LLM generator (no second OpenRouter client). No RAG (the teor is self-contained).
	// The store OVERWRITES on each call ("Gerar novamente"); the use case degrades to an empty
	// analysis when taskGenerator is nil, so the endpoint always answers 200 (never a boot fail).
	analiseUC := acquisition.NewAnaliseUseCase(
		acquisition.NewReadUseCase(acquisitionRepo),
		advisory.NewTemplateComposer(),
		taskGenerator,
		acquisition.NewAnaliseStore(uow),
		// QUALITY: providências viram tasks reais no calendário — falso positivo custa.
		cfg.OpenRouterModelQuality,
	)
	// Providência actions (aprovar/descartar) on the analysis card — flip one suggested
	// providência's status by index over the unit of work. The real task is created by the FE
	// via POST /v1/tasks (deadline slice) before the aprovar call, so this stays decoupled.
	providenciaUC := acquisition.NewProvidenciaActionUseCase(uow)
	// OAB name lookup (GET /v1/acquisition/oab-lookup, onboarding/Termos best-effort
	// auto-fill): its own DJENConnector instance — no free public OAB registry API
	// exists (the OAB's own CNA web service requires an institutional Authentication
	// Key we don't have; see oab_lookup.go), so this reuses the SAME public consulta
	// the worker's backfill uses, just a single on-demand page. Same env-derived
	// proxy/rate/page-size options as worker-ingestao, but a SEPARATE process (a
	// different binary/container), so its rate limiter/cooldown cannot be shared with
	// the worker's — an accepted v0 tradeoff given this is a low-volume, occasional
	// call (one OAB add at a time), not a bulk sweep.
	djenLookupOpts := []acquisition.DJENOption{}
	if cfg.DJENProxyURL != "" {
		proxyURL, perr := url.Parse(cfg.DJENProxyURL)
		if perr != nil {
			return fmt.Errorf("parse DJEN_PROXY_URL: %w", perr)
		}
		djenLookupOpts = append(djenLookupOpts, acquisition.WithDJENProxy(proxyURL))
	}
	if cfg.DJENRatePerMinute > 0 {
		djenLookupOpts = append(djenLookupOpts, acquisition.WithDJENRatePerMinute(cfg.DJENRatePerMinute))
	}
	if cfg.DJENPageSize > 0 {
		djenLookupOpts = append(djenLookupOpts, acquisition.WithDJENPageSize(cfg.DJENPageSize))
	}
	djenLookupConnector := acquisition.NewDJENConnector(djenLookupOpts...)

	// Acquisition wiring: the slice owns the domain; the binary only assembles it
	// (repo + shared outbox + unit of work → write use case; the same repo backs the
	// read use case for the processos/intimações screen reads; the same read use case
	// + the resumoUC above back the AI process summary). Activation is
	// entitlement-gated (the tenant's ACTIVE-process ceiling, read from billing), so an
	// over-limit onboarding is refused at the edge.
	acquisitionHandler := acquisition.NewHandler(
		acquisition.NewUseCase(acquisitionRepo, events.NewOutbox(), uow,
			acquisition.WithActivationEntitlementChecker(acquisitionEntitlement)),
		acquisition.NewReadUseCase(acquisitionRepo,
			acquisition.WithCaptureDailyTime(cfg.CaptureDailyTime)),
		resumoUC,
		analiseUC,
		providenciaUC,
		djenLookupConnector,
	)
	// The suggester also captures provenance (feedback loop, camada 1): every suggestion batch
	// is persisted (best-effort) via its own tx so the confirm can later diff it against the
	// lawyer's confirmed tasks. The model string is recorded alongside the prompt_version. It
	// also write-throughs the "O que aconteceu"/"O que fazer" summary (migration 0036) the FIRST
	// time a prazo is summarized, so later opens serve the cache instead of asking the LLM again.
	deadlineSuggestUC := deadline.NewSuggestUseCase(
		deadlineReadUC, advisory.NewTemplateComposer(), taskGenerator,
		deadline.NewSuggestionStore(uow), deadline.NewAISummaryStore(uow),
		// FAST: sugestão de tasks é background/on-demand curta — rápido melhor.
		cfg.OpenRouterModelFast,
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

	// Onboarding wiring: the post-signup activation widget (progress read +
	// dismiss write). Read-only over other slices' tables via its own SQL — no
	// domain repo/outbox from those slices is injected here, only pool + the
	// shared unit of work.
	onboardingHandler := onboarding.NewHandler(onboarding.NewUseCase(onboarding.NewRepository(pool), uow))

	// Storage is optional at v0: only wired when S3 is fully configured. The Documentos
	// slice (Fatia 1) consumes it — the presigned upload/download — so when storage is
	// present the document write use case + handler are built off it (repo + storage +
	// shared outbox + unit of work), mirroring the deadline composition. Built to fail
	// fast on bad credentials at boot and logged as ready. Without S3 the slice stays
	// unmounted (documentHandler nil → the router nil-guard skips it).
	var storageClient *storage.Client
	var documentHandler *document.Handler
	if cfg.S3Enabled() {
		var err error
		storageClient, err = storage.New(ctx, storage.Options{
			Endpoint:     cfg.S3Endpoint,
			Region:       cfg.S3Region,
			Bucket:       cfg.S3Bucket,
			AccessKey:    cfg.S3AccessKey,
			SecretKey:    cfg.S3SecretKey,
			UsePathStyle: cfg.S3UsePathStyle,
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

	// Certificate (Assinatura Fatia 1): cadastro do .pfx A1 ICP-Brasil.
	// Envelope encryption: DEK aleatória (AES-256-GCM local) por cert, wrapped
	// pelo GCP KMS. Sem GCP_KMS_KEY_NAME o slice fica desmontado; o api segue.
	// Credenciais GCP: em PaaS (Railway/Fly/Render) recebemos o JSON como env
	// base64 (GCP_KMS_CREDENTIALS_JSON) e materializamos em disco antes de
	// instanciar o SDK — o ADC lê de GOOGLE_APPLICATION_CREDENTIALS. Em dev
	// local (Docker Compose) já vem via volume mount, então a var já aponta
	// pro path certo e este ramo é no-op.
	var certificateHandler *certificate.Handler
	var certCipher certificate.Cipher
	var certUC *certificate.UseCase // exposto pra outros slices (ex.: draft.Sign)
	if cfg.GCPKMSKeyName != "" {
		if err := materializeGCPCredentials(cfg.GCPKMSCredentialsJSON); err != nil {
			return fmt.Errorf("materialize gcp creds: %w", err)
		}
		cipher, err := certificate.NewEnvelopeCipher(ctx, cfg.GCPKMSKeyName)
		if err != nil {
			return fmt.Errorf("init cert envelope cipher: %w", err)
		}
		certCipher = cipher
		certUC = certificate.NewUseCase(certificate.NewRepository(), uow, cipher, events.NewOutbox())
		certificateHandler = certificate.New(certUC)
		logger.Info("certificate slice ready", "kms_key", cfg.GCPKMSKeyName)
	} else {
		logger.Warn("GCP_KMS_KEY_NAME unset — certificate endpoints will be disabled",
			"impact", "GET/POST/DELETE /v1/certificates* will return 404 until GCP_KMS_KEY_NAME and GCP_KMS_CREDENTIALS_JSON are configured")
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
	draftOpts := []draft.Option{draft.WithOutbox(events.NewOutbox())}
	if storageClient != nil {
		draftOpts = append(draftOpts, draft.WithStorage(storageClient))
		// Fatia 2b: PDF assinado passa pelo BE (server-side PutBytes). Reusa
		// o mesmo storage.Client — Put/Get direto (bypass presigned).
		draftOpts = append(draftOpts, draft.WithPDFStorage(storageClient))
	}
	if certUC != nil {
		// certUC.NewSigner satisfaz o port draft.CertSigner via adapter direto —
		// mesma assinatura, só encaminha a chamada. Isolamento cross-slice.
		draftOpts = append(draftOpts, draft.WithCertSigner(certSignerFunc(certUC.NewSigner)))
		// certCipher (KMS envelope) satisfaz o port draft.SecretVault para
		// cifrar login/senha e-SAJ. Structural typing — o adapter converte
		// certificate.Envelope ↔ draft.Envelope (campos idênticos).
		draftOpts = append(draftOpts, draft.WithSecretVault(secretVaultAdapter{certCipher}))
	}
	if cfg.TSAURL != "" {
		// PAdES-T: carimbo de tempo RFC 3161 embutido na assinatura. Sem esta
		// var, o Sign gera PAdES-BASIC (data da assinatura = clock do assinante,
		// não confiável). Provedor típico: http://freetsa.org/tsr (dev).
		draftOpts = append(draftOpts, draft.WithTSAURL(cfg.TSAURL))
		logger.Info("PAdES-T habilitado", "tsa_url", cfg.TSAURL)
	}
	draftUC := draft.NewUseCase(uow, draftRepo, draftOpts...)
	draftTrigger := draft.NewTriggerUseCase(uow, draftRepo, events.NewOutbox())
	draftHandler := draft.NewHandler(draftUC).
		WithGenerator(draftTrigger).
		WithLister(draftUC).
		WithExport(draftUC)
	if storageClient != nil {
		draftHandler = draftHandler.WithStorage(storageClient)
	}

	// Streaming da geração (Fatia 2 do streaming). Subscriber compartilha o
	// mesmo Redis do resto da app; getSaga consulta o read model do próprio
	// slice pra saber quando fechar a stream (DRAFTED/FAILED).
	draftHandler = draftHandler.WithGenerationStream(
		pubsub.NewRedisPubSub(pubsubClient),
		func(ctx context.Context, tenantID, draftID string) (string, error) {
			view, err := draftUC.GetDetail(ctx, tenantID, draftID)
			if err != nil {
				return "", err
			}
			return view.SagaState, nil
		},
	)
	// Stream token store — usado pelo issuer (POST /stream-token) e pelo
	// middleware específico da rota SSE (fluxo alternativo ao Bearer).
	streamTokenStore := draft.NewRedisStreamTokenStore(pubsubClient)
	draftHandler = draftHandler.WithStreamTokenStore(streamTokenStore)

	// Chat use case (Fatia 3b): reuses taskGenerator + resumeEmbedder + advisory
	// composer already instantiated above. nil generator / nil embedder → degraded
	// (ErrIANotConfigured / grounded=false) — never a boot failure.
	var chatEmbedder draft.VoyageEmbedder
	if resumeEmbedder != nil {
		chatEmbedder = resumeEmbedder
	}
	// RAG cache: reduz p95 de teses/chat/review/iterate quando o mesmo query
	// text é chamado várias vezes em janela de 5min (troca de tipo na Partida,
	// teses→generate na mesma sessão). Nil-safe: se pubsubClient falha, o cache
	// vira nil e as use cases seguem como antes (Voyage + pgvector direto).
	ragCache := draft.NewRAGCache(pubsubClient, 5*time.Minute)
	chatUC := draft.NewChatUseCase(draft.ChatUseCaseParams{
		UoW:      uow,
		Repo:     draftRepo,
		Gen:      taskGenerator,
		Emb:      chatEmbedder,
		Search:   indexing.SearchDeps{Pool: pool},
		RAGCache: ragCache,
		Composer: advisory.NewTemplateComposer(),
		// FAST: chat é loop de pergunta/resposta — UX exige rapidez.
		Model: cfg.OpenRouterModelFast,
	})
	draftHandler = draftHandler.WithChat(chatUC)

	// Review use case (Fatia 3 — Revisar síncrono): same deps as ChatUseCase (shared
	// generator, embedder, search, composer, model). Revisar produces AI suggestions
	// (findings) over the advogado's current minuta, grounded in the case corpus.
	// nil generator → ErrIANotConfigured (422) — never a boot failure.
	reviewUC := draft.NewReviewUseCase(draft.ReviewUseCaseParams{
		UoW:      uow,
		Reader:   draftRepo,
		Writer:   draftRepo,
		Gen:      taskGenerator,
		Emb:      chatEmbedder,
		Search:   indexing.SearchDeps{Pool: pool},
		RAGCache: ragCache,
		Composer: advisory.NewTemplateComposer(),
		// QUALITY: findings viram edits no texto — precisão importa.
		Model: cfg.OpenRouterModelQuality,
	})
	draftHandler = draftHandler.WithReviewer(reviewUC)

	// Theses use case (Fatia 5 — Sugerir Teses síncrono): same deps as ReviewUseCase,
	// minus a writer (STATELESS — read+LLM only, no saga_state transition, no
	// persisted row). nil generator → ErrIANotConfigured (422) — never a boot failure.
	thesesUC := draft.NewThesesUseCase(draft.ThesesUseCaseParams{
		UoW:      uow,
		Reader:   draftRepo,
		Gen:      taskGenerator,
		Emb:      chatEmbedder,
		Search:   indexing.SearchDeps{Pool: pool},
		RAGCache: ragCache,
		Composer: advisory.NewTemplateComposer(),
		// FAST: teses re-disparam a cada troca de tipo no dropdown — velocidade > exaustividade.
		Model: cfg.OpenRouterModelFast,
	})
	draftHandler = draftHandler.WithTheses(thesesUC)

	// Vault is optional: only instantiated when VAULT_KEK_BASE64 is present. When
	// absent the court-credential slice stays unmounted (S2) — same optional-adapter
	// convention as document/S3. vault.New validates the KEK eagerly so a malformed
	// key fails at boot, not mid-request.
	var vlt *vault.Vault
	if cfg.VaultKEK != "" {
		v, err := vault.New(cfg.VaultKEK)
		if err != nil {
			return fmt.Errorf("init vault: %w", err)
		}
		vlt = v
		logger.Info("vault configured")
	} else {
		logger.Warn("VAULT_KEK_BASE64 unset — court credential endpoints will be disabled")
	}

	// Court (execução judicial, fatia 1 — autenticação + MFA automatizado):
	// precisa do certUC (assina/decifra o certificado A1 do advogado) E do vault
	// (sela a seed TOTP capturada no enrollment automatizado) — sem os dois, a
	// conexão com o tribunal não tem como autenticar de verdade, então o slice
	// fica desmontado (mesmo padrão optional-adapter do document/S3 e do vault
	// acima), não half-up.
	var courtHandler *court.Handler
	if certUC != nil && vlt != nil {
		courtUC := court.NewUseCase(court.NewRepository(), uow, vlt, events.NewOutbox())
		// nil DocumentWriter: cmd/api only ever calls Connect/EnrollMFA
		// synchronously (courtHandler's HTTP endpoints) — FetchAutos, the only
		// method that downloads documents, runs exclusively in cmd/worker-court.
		courtUC.RegisterProvider("EPROC", court.NewEprocProvider(courtCertSignerFunc(certUC.NewSigner), nil))
		courtHandler = court.New(courtUC)
		logger.Info("court slice ready", "providers", []string{"EPROC"})
	} else {
		logger.Warn("GCP_KMS_KEY_NAME or VAULT_KEK_BASE64 unset — court-connection endpoints will be disabled")
	}

	// Iterate use case (Peça v2 — POST /v1/pecas/:id/iterate síncrono). Reusa
	// o mesmo generator + embedder + composer das outras fatias de IA. nil
	// generator → 422 em runtime; nunca falha de boot. Stateless (no writer —
	// FE aplica cada change via PATCH /v1/pecas/:id).
	iterateUC := draft.NewIterateUseCase(draft.IterateParams{
		UoW:      uow,
		Reader:   draftRepo,
		Composer: advisory.NewTemplateComposer(),
		Gen:      taskGenerator,
		// QUALITY: cada iteração edita texto real que vai no PDF — erro custa caro.
		Model:    cfg.OpenRouterModelQuality,
		Embedder: chatEmbedder,
		Search:   indexing.SearchDeps{Pool: pool},
		RAGCache: ragCache,
	})
	draftHandler = draftHandler.WithIterator(iterateUC)

	// 5. Router — the testable seam; no I/O happens here.
	app := newRouter(routerDeps{
		logger:               logger,
		rateLimitMax:         cfg.RateLimitMax,
		rateLimitWebhookMax:  cfg.RateLimitWebhookMax,
		rateLimitWindow:      cfg.RateLimitWindow(),
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
		certificate:          certificateHandler,
		court:                courtHandler,
		onboarding:           onboardingHandler,
		vault:                vlt,
		streamTokenStore:     streamTokenStore,
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
			if certCipher != nil {
				if err := certCipher.Close(); err != nil {
					errs = append(errs, fmt.Errorf("close cert cipher: %w", err))
				}
			}
			return errors.Join(errs...)
		},
	)

	return nil
}

// certSignerFunc adapta a assinatura de certificate.UseCase.NewSigner ao port
// draft.CertSigner. O UC devolve certificate.SignerInfo, que é convertido pro
// draft.SignerInfo local — evita import cíclico (draft não conhece certificate).
type certSignerFunc func(ctx context.Context, tenantID, id string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, certificate.SignerInfo, error)

func (f certSignerFunc) NewSigner(ctx context.Context, tenantID, id string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, draft.SignerInfo, error) {
	signer, leaf, chain, info, err := f(ctx, tenantID, id)
	return signer, leaf, chain, draft.SignerInfo{OAB: info.OAB, SubjectCN: info.SubjectCN}, err
}

// courtCertSignerFunc adapta certificate.UseCase.NewSigner ao port court.CertSigner —
// mesma técnica de certSignerFunc (mesmo import cíclico a evitar), só descartando
// certificate.SignerInfo, que o slice court não precisa (EprocProvider só monta um
// tls.Certificate a partir do signer+leaf+chain).
type courtCertSignerFunc func(ctx context.Context, tenantID, id string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, certificate.SignerInfo, error)

func (f courtCertSignerFunc) NewSigner(ctx context.Context, tenantID, id string) (crypto.Signer, *x509.Certificate, []*x509.Certificate, error) {
	signer, leaf, chain, _, err := f(ctx, tenantID, id)
	return signer, leaf, chain, err
}

// secretVaultAdapter adapta certificate.Cipher (KMS envelope: Seal/Open/Close)
// ao port draft.SecretVault. Os Envelopes têm campos idênticos; o adapter só
// converte os tipos nomeados para evitar import cíclico.
type secretVaultAdapter struct {
	cipher certificate.Cipher
}

func (a secretVaultAdapter) Seal(ctx context.Context, plaintext []byte) (*draft.Envelope, error) {
	env, err := a.cipher.Seal(ctx, plaintext)
	if err != nil {
		return nil, err
	}
	return &draft.Envelope{
		Ciphertext: env.Ciphertext,
		Nonce:      env.Nonce,
		WrappedDEK: env.WrappedDEK,
		KEKRef:     env.KEKRef,
	}, nil
}

func (a secretVaultAdapter) Open(ctx context.Context, env *draft.Envelope) ([]byte, error) {
	return a.cipher.Open(ctx, &certificate.Envelope{
		Ciphertext: env.Ciphertext,
		Nonce:      env.Nonce,
		WrappedDEK: env.WrappedDEK,
		KEKRef:     env.KEKRef,
	})
}

func (a secretVaultAdapter) Close() error {
	return a.cipher.Close()
}

// materializeGCPCredentials adapts PaaS-style env-only credentials into the
// disk-file shape the GCP SDK expects. When GCP_KMS_CREDENTIALS_JSON is set
// (base64 of the service-account JSON), decode it, write to /tmp/gcp-kms.json
// and overwrite GOOGLE_APPLICATION_CREDENTIALS to point there. When it is
// empty, do nothing — the caller relies on GOOGLE_APPLICATION_CREDENTIALS
// already pointing to a mounted file (dev via Docker volume). This is the
// standard "env-only" workaround for Railway/Fly/Render which do not allow
// mounting arbitrary files.
func materializeGCPCredentials(b64 string) error {
	if b64 == "" {
		return nil // caller relies on the mounted file path already in env
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return fmt.Errorf("decode GCP_KMS_CREDENTIALS_JSON base64: %w", err)
	}
	path := "/tmp/gcp-kms.json"
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write GCP creds to %s: %w", path, err)
	}
	if err := os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path); err != nil {
		return fmt.Errorf("set GOOGLE_APPLICATION_CREDENTIALS: %w", err)
	}
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
