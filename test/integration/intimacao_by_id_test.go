//go:build integration

// GET /v1/intimacoes/:id read integration test — proves the deep-link read model
// (GetIntimacao) against a real Postgres: a seeded intimation round-trips by id, and
// the tenant scope (barrier 1) turns a foreign tenant's id into the typed 404. Reuses
// the acquisition read use case + repo like the other read tests in this package.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/apperr"
)

// TestGetIntimacao_ByID_RoundTrips seeds one intimation and reads it back by id: the
// view carries the intimation's own id plus the joined court record's cnj/court/degree
// (the same IntimacaoView the inbox list projects).
func TestGetIntimacao_ByID_RoundTrips(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inti-byid", 0)
	rec, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0004567-11.2023.8.26.0001")
	intiID := seedIntimationReturningID(t, pool, tenantID, caseID, rec)

	got, err := uc.Intimacao(ctx, tenantID, intiID)
	if err != nil {
		t.Fatalf("Intimacao: %v", err)
	}
	if got.ID != intiID {
		t.Errorf("id = %q, want %q", got.ID, intiID)
	}
	if got.CNJNumber != "0004567-11.2023.8.26.0001" {
		t.Errorf("cnj_number = %q, want the joined record's number", got.CNJNumber)
	}
	if got.Court != "TJSP" || got.Degree != "G1" {
		t.Errorf("court/degree = %q/%q, want TJSP/G1", got.Court, got.Degree)
	}
}

// TestGetIntimacao_CrossTenant_NotFound proves the tenant scope: reading the same id
// under another tenant is the typed ErrIntimationNotFound (→ 404), never a leak.
func TestGetIntimacao_CrossTenant_NotFound(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inti-owner", 0)
	rec, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0009999-11.2023.8.26.0001")
	intiID := seedIntimationReturningID(t, pool, tenantID, caseID, rec)

	if _, err := uc.Intimacao(ctx, uuid.NewString(), intiID); !errors.Is(err, acquisition.ErrIntimationNotFound) {
		t.Errorf("cross-tenant Intimacao err = %v, want ErrIntimationNotFound", err)
	}

	// A non-uuid id is client input → the typed KindInvalid (→ 400), not a 404 or 500.
	_, err := uc.Intimacao(ctx, tenantID, "not-a-uuid")
	var appErr *apperr.AppError
	if !errors.As(err, &appErr) || appErr.Kind != apperr.KindInvalid {
		t.Errorf("non-uuid Intimacao err = %v, want KindInvalid", err)
	}
}

// TestGetIntimacao_Detail_FullContentAndJudgingBody proves the deep-link detail view
// carries the FULL teor (not the truncated preview), the court record's judging_body, the
// recipients jsonb and the triagem user_status — the fields the inbox list omits.
func TestGetIntimacao_Detail_FullContentAndJudgingBody(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	uc := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-inti-detail", 0)
	rec, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0004567-11.2023.8.26.0777")

	// Set a judging_body on the record (seedCourtRecordCNJ leaves it NULL).
	if _, err := pool.Exec(ctx,
		`UPDATE court_record SET judging_body = '3ª Vara Cível' WHERE id = $1`, rec); err != nil {
		t.Fatalf("set judging_body: %v", err)
	}

	// A long teor (> the 500-rune preview cap) so the detail's full content is distinguishable.
	longContent := ""
	for range 600 {
		longContent += "x"
	}
	var intiID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source, recipients, user_status)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', $5, 'DJEN',
		         '[{"name":"Fulano","matched":true}]', 'RESOLVED')
		 RETURNING id::text`,
		tenantID, caseID, rec, uuid.NewString(), longContent).Scan(&intiID); err != nil {
		t.Fatalf("seed intimation: %v", err)
	}

	got, err := uc.Intimacao(ctx, tenantID, intiID)
	if err != nil {
		t.Fatalf("Intimacao: %v", err)
	}
	if got.Content != longContent {
		t.Errorf("detail Content len = %d, want the full %d-rune teor (untruncated)", len(got.Content), len(longContent))
	}
	if len(got.ContentPreview) != 500 {
		t.Errorf("ContentPreview len = %d, want 500 (still truncated in the embedded list view)", len(got.ContentPreview))
	}
	if got.JudgingBody != "3ª Vara Cível" {
		t.Errorf("JudgingBody = %q, want the joined court_record.judging_body", got.JudgingBody)
	}
	if got.UserStatus != acquisition.IntimationUserStatusResolved {
		t.Errorf("UserStatus = %q, want RESOLVED", got.UserStatus)
	}
	// jsonb normalizes whitespace, so compare on the decoded value rather than byte-exact.
	var recipients []map[string]any
	if err := json.Unmarshal(got.Recipients, &recipients); err != nil {
		t.Fatalf("Recipients is not valid json: %v (raw: %s)", err, got.Recipients)
	}
	if len(recipients) != 1 || recipients[0]["name"] != "Fulano" || recipients[0]["matched"] != true {
		t.Errorf("Recipients decoded = %v, want one matched Fulano", recipients)
	}
}

// seedIntimationReturningID inserts one intimation for the record (no discovering
// window — sync_run_id stays NULL) and returns its id, for the by-id read.
func seedIntimationReturningID(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, recordID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO intimation
		   (tenant_id, case_id, court_record_id, hash, made_available_at, published_at,
		    deadline_start_at, content, source)
		 VALUES ($1, $2, $3, $4, '2026-01-05', '2026-01-06', '2026-01-07', 'teor', 'DJEN')
		 RETURNING id::text`,
		tenantID, caseID, recordID, uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("seed intimation: %v", err)
	}
	return id
}
