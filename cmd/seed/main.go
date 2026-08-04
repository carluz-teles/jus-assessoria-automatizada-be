// Command seed provisions a tenant + user (mirroring a Clerk org/user, since the
// local stack has no Clerk webhook) and runs the REAL acquisition pipeline for a
// set of OABs against the local database: DJEN discovery → persistence →
// intimations, then DATAJUD enrichment (grade + placeholder+merge + movimentos)
// for a capped number of discovered records. It is a dev/demo tool — the data it
// writes is produced by the real connectors/parsers/repo (no mock), just driven
// synchronously instead of through the asynq workers, with the entitlement gate
// left unlimited (so it needs no billing subscription).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

var (
	oabs       = []string{"SP347019", "SP321511", "MG199303"}
	windowFrom = "2026-07-30"
	windowTo   = "2026-07-31"
	enrichCap  = 8
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

	// ── provision tenant + user (idempotent) + a DJEN integration ──────────────
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
	scopeJSON, _ := json.Marshal(map[string][]string{"oab": oabs})
	var integrationID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO integration (tenant_id, source, scope, status) VALUES ($1,'DJEN',$2,'ACTIVE')
		 ON CONFLICT (tenant_id, source) DO UPDATE SET scope=EXCLUDED.scope
		 RETURNING id::text`, tenantID, scopeJSON).Scan(&integrationID); err != nil {
		return fmt.Errorf("seed integration: %w", err)
	}
	log.Printf("provisioned tenant=%s user=%s integration=%s", tenantID, clerkUser, integrationID)

	// ── real pipeline: DJEN discovery ──────────────────────────────────────────
	repo := acquisition.NewRepository(pool)
	outbox := events.NewOutbox()
	uow := database.NewUnitOfWork(pool)
	orch := acquisition.NewOrchestrator()
	orch.Register(acquisition.SourceDJEN, acquisition.NewDJENConnector())
	orch.Register(acquisition.SourceDATAJUD, acquisition.NewDATAJUDConnector())
	cal := calendar.New(calendar.NewStore(pool))
	parser := acquisition.ParserSet{acquisition.NewDJENParser(cal), acquisition.NewDATAJUDParser()}

	sync := acquisition.NewSyncUseCase(repo, outbox, uow, orch, parser)
	enrich := acquisition.NewEnrichmentUseCase(repo, outbox, uow, orch, parser)

	syncEv := acquisition.SyncRequested{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: integrationID},
		BackfillJobID: uuid.NewString(),
		TenantID:      tenantID,
		IntegrationID: integrationID,
		Source:        acquisition.SourceDJEN,
		WindowFrom:    windowFrom,
		WindowTo:      windowTo,
		Scope:         acquisition.Scope{OAB: oabs},
	}
	log.Printf("DJEN discovery: OABs=%v window=%s..%s", oabs, windowFrom, windowTo)
	if err := sync.OnSyncRequested(ctx, syncEv); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	discovered := count(ctx, pool, `SELECT count(*) FROM court_record WHERE tenant_id=$1`, tenantID)
	intims := count(ctx, pool, `SELECT count(*) FROM intimation WHERE tenant_id=$1`, tenantID)
	log.Printf("discovered: %d court records, %d intimations", discovered, intims)

	// ── real pipeline: DATAJUD enrichment for the first N observed records ──────
	rows, err := pool.Query(ctx,
		`SELECT payload FROM outbox WHERE type=$1 ORDER BY id LIMIT $2`,
		acquisition.TypeCourtRecordObserved, enrichCap)
	if err != nil {
		return fmt.Errorf("read outbox: %w", err)
	}
	var observed []acquisition.CourtRecordObserved
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return err
		}
		var ev acquisition.CourtRecordObserved
		if err := json.Unmarshal(payload, &ev); err != nil {
			rows.Close()
			return err
		}
		observed = append(observed, ev)
	}
	rows.Close()

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
