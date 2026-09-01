// Command seed provisions a tenant + user (mirroring a Clerk org/user, since the
// local stack has no Clerk webhook) and runs the REAL acquisition pipeline for a
// set of OABs against the local database: ActivateIntegration → the backfill
// listener's onboarding fan-out (backfill_job + N weekly sync_requested slices,
// 30-day horizon) → DJEN discovery per slice → persistence → intimations, then
// DATAJUD enrichment (grade + placeholder+merge + movimentos) for a capped number
// of discovered records. It is a dev/demo tool — the data it writes is produced by
// the real use cases/connectors/parsers/repo (no mock), just driven synchronously
// instead of through the asynq workers, with the entitlement gate left unlimited
// (so it needs no billing subscription).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

var (
	oabs      = []string{"SP347019", "SP321511", "MG198988"}
	enrichCap = 8
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	clerkOrg := envOr("CLERK_ORG_ID", "")
	clerkUser := envOr("CLERK_USER_ID", "")
	email := envOr("USER_EMAIL", "demo@atjud.com.br")
	orgName := envOr("ORG_NAME", "AtJud")
	if dsn == "" || clerkOrg == "" || clerkUser == "" {
		return fmt.Errorf("need DATABASE_URL, CLERK_ORG_ID, CLERK_USER_ID env")
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("pool: %w", err)
	}
	defer pool.Close()

	// ── provision tenant + user (idempotent) ───────────────────────────────────
	var tenantID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenant (clerk_org_id, name) VALUES ($1,$2)
		 ON CONFLICT (clerk_org_id) DO UPDATE SET name=EXCLUDED.name
		 RETURNING id::text`, clerkOrg, orgName).Scan(&tenantID); err != nil {
		return fmt.Errorf("seed tenant: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (clerk_user_id, tenant_id, email, role) VALUES ($1,$2,$3,'ADMIN')
		 ON CONFLICT (clerk_user_id) DO UPDATE SET tenant_id=EXCLUDED.tenant_id, email=EXCLUDED.email`,
		clerkUser, tenantID, email); err != nil {
		return fmt.Errorf("seed app_user: %w", err)
	}
	log.Printf("provisioned tenant=%s user=%s", tenantID, clerkUser)

	// ── real pipeline wiring ────────────────────────────────────────────────────
	repo := acquisition.NewRepository(pool)
	outbox := events.NewOutbox()
	uow := database.NewUnitOfWork(pool)
	orch := acquisition.NewOrchestrator()
	orch.Register(acquisition.SourceDJEN, acquisition.NewDJENConnector())
	orch.Register(acquisition.SourceDATAJUD, acquisition.NewDATAJUDConnector())
	cal := calendar.New(calendar.NewStore(pool))
	parser := acquisition.ParserSet{acquisition.NewDJENParser(cal), acquisition.NewDATAJUDParser()}

	activate := acquisition.NewUseCase(repo, outbox, uow)
	backfill := acquisition.NewBackfillUseCase(repo, outbox, uow) // no history matcher: legacy per-OAB path
	sync := acquisition.NewSyncUseCase(repo, outbox, uow, orch, parser)
	enrich := acquisition.NewEnrichmentUseCase(repo, outbox, uow, orch, parser)

	// ── ActivateIntegration: upserts `integration`, publishes integration_activated ──
	integ, err := activate.ActivateIntegration(ctx, tenantID, acquisition.Scope{OAB: oabs})
	if err != nil {
		return fmt.Errorf("activate integration: %w", err)
	}
	log.Printf("integration activated: id=%s oabs=%v", integ.ID, oabs)

	activatedEv, err := lastEvent[acquisition.IntegrationActivated](ctx, pool, acquisition.TypeIntegrationActivated, tenantID)
	if err != nil {
		return fmt.Errorf("read integration_activated: %w", err)
	}

	// ── OnIntegrationActivated: opens backfill_job + emits N weekly sync_requested ──
	if err := backfill.OnIntegrationActivated(ctx, activatedEv); err != nil {
		return fmt.Errorf("backfill: %w", err)
	}

	syncEvs, err := allEvents[acquisition.SyncRequested](ctx, pool, acquisition.TypeSyncRequested, tenantID)
	if err != nil {
		return fmt.Errorf("read sync_requested: %w", err)
	}
	log.Printf("DJEN discovery: OABs=%v, %d slices over the 30-day onboarding horizon", oabs, len(syncEvs))
	for _, ev := range syncEvs {
		if err := sync.OnSyncRequested(ctx, ev); err != nil {
			return fmt.Errorf("sync slice %s..%s: %w", ev.WindowFrom, ev.WindowTo, err)
		}
	}
	discovered := count(ctx, pool, `SELECT count(*) FROM court_record WHERE tenant_id=$1`, tenantID)
	intims := count(ctx, pool, `SELECT count(*) FROM intimation WHERE tenant_id=$1`, tenantID)
	log.Printf("discovered: %d court records, %d intimations", discovered, intims)

	// ── real pipeline: DATAJUD enrichment for the first N observed records ──────
	observed, err := firstEvents[acquisition.CourtRecordObserved](ctx, pool, acquisition.TypeCourtRecordObserved, enrichCap, tenantID)
	if err != nil {
		return fmt.Errorf("read court_record_observed: %w", err)
	}

	log.Printf("enriching %d records via DATAJUD...", len(observed))
	for _, ev := range observed {
		if err := enrich.OnCourtRecordObserved(ctx, ev); err != nil {
			return fmt.Errorf("enrich %s: %w", ev.CNJNumber, err)
		}
	}

	graded := count(ctx, pool, `SELECT count(*) FROM court_record WHERE tenant_id=$1 AND degree<>'UNKNOWN'`, tenantID)
	docket := count(ctx, pool, `SELECT count(*) FROM docket_entry de JOIN court_record cr ON cr.id=de.court_record_id WHERE cr.tenant_id=$1`, tenantID)
	log.Printf("DONE: %d processos (%d graded), %d intimações, %d andamentos — tenant=%s",
		discovered, graded, intims, docket, tenantID)
	return nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func count(ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int {
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		log.Printf("count failed: %v", err)
	}
	return n
}

// This dev box's outbox accumulates rows across many past seed/demo/QA sessions
// (other tenants, the always-running national firehose ingestion) — every read
// below filters by payload->>'tenant_id' so a fresh run never picks up another
// tenant's leftover events.

// lastEvent reads the most recently written outbox row of the given type for
// tenantID and decodes its payload — used right after an ActivateIntegration
// call, where exactly one fresh integration_activated row is expected.
func lastEvent[T any](ctx context.Context, pool *pgxpool.Pool, eventType, tenantID string) (T, error) {
	var zero T
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE type=$1 AND payload->>'tenant_id'=$2 ORDER BY id DESC LIMIT 1`,
		eventType, tenantID,
	).Scan(&payload); err != nil {
		return zero, err
	}
	var ev T
	if err := json.Unmarshal(payload, &ev); err != nil {
		return zero, err
	}
	return ev, nil
}

// allEvents reads every outbox row of the given type for tenantID, oldest first.
func allEvents[T any](ctx context.Context, pool *pgxpool.Pool, eventType, tenantID string) ([]T, error) {
	rows, err := pool.Query(ctx,
		`SELECT payload FROM outbox WHERE type=$1 AND payload->>'tenant_id'=$2 ORDER BY id`,
		eventType, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []T
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var ev T
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, rows.Err()
}

// firstEvents reads up to cap outbox rows of the given type for tenantID, oldest first.
func firstEvents[T any](ctx context.Context, pool *pgxpool.Pool, eventType string, cap int, tenantID string) ([]T, error) {
	rows, err := pool.Query(ctx,
		`SELECT payload FROM outbox WHERE type=$1 AND payload->>'tenant_id'=$2 ORDER BY id LIMIT $3`,
		eventType, tenantID, cap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var evs []T
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var ev T
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, err
		}
		evs = append(evs, ev)
	}
	return evs, rows.Err()
}
