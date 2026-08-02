//go:build integration

// Onboarding profile integration tests — prove PUT /organization/profile against a
// real Postgres: the company columns and the address jsonb are written and the
// org_profile_updated event lands in the SAME transaction (atomic; a failed publish
// rolls the whole write back), the onboarding gate is stamped exactly once
// (idempotent across replays), and the write is scoped to the principal's own
// tenant (the tenant table has no RLS, so the WHERE id filter is the barrier).
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/identity"
	"github.com/jusassessoria/platform/lib/database"
)

// tenantProfileRow is the raw tenant profile as read by the owner (RLS bypassed).
type tenantProfileRow struct {
	cnpj      *string
	legalName *string
	tradeName *string
	address   []byte
	onboarded *time.Time
}

// readTenantProfile reads the profile columns for a tenant directly, so the test
// asserts what actually landed on the row rather than trusting the returned entity.
func readTenantProfile(t *testing.T, pool *pgxpool.Pool, tenantID string) tenantProfileRow {
	t.Helper()
	var row tenantProfileRow
	if err := pool.QueryRow(context.Background(),
		`SELECT cnpj, legal_name, trade_name, address, onboarding_completed_at
		   FROM tenant WHERE id = $1`, tenantID,
	).Scan(&row.cnpj, &row.legalName, &row.tradeName, &row.address, &row.onboarded); err != nil {
		t.Fatalf("read tenant profile: %v", err)
	}
	return row
}

// countProfileOutbox counts unpublished org_profile_updated events for a tenant
// (the outbox aggregate id is the tenant id).
func countProfileOutbox(t *testing.T, pool *pgxpool.Pool, tenantID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox
		  WHERE type = $1 AND published_at IS NULL AND aggregate_id::text = $2`,
		identity.TypeOrgProfileUpdated, tenantID).Scan(&n); err != nil {
		t.Fatalf("count profile outbox: %v", err)
	}
	return n
}

func sampleProfile(tradeName string) identity.OrgProfile {
	return identity.OrgProfile{
		CNPJ:      "12345678000195",
		LegalName: "Escritório LTDA",
		TradeName: tradeName,
		Address: identity.Address{
			CEP:        "01311902",
			Logradouro: "Av Paulista",
			Numero:     "1000",
			Cidade:     "São Paulo",
			UF:         "SP",
		},
	}
}

// AC7: the profile columns + address jsonb are written AND the org_profile_updated
// event is committed in the same transaction; the onboarding gate is stamped once
// and a replay does not move it (idempotent).
func TestIdentity_UpdateOrgProfile_ColumnsAndOutboxSameTx(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-profile-1", 0)

	uc := newIdentityUC(pool)

	// Before onboarding: every profile column is NULL.
	if before := readTenantProfile(t, pool, tenantID); before.cnpj != nil || before.onboarded != nil {
		t.Fatalf("profile not empty before onboarding: %+v", before)
	}

	saved, err := uc.UpdateOrgProfile(ctx, tenantID, sampleProfile("Escritório"))
	if err != nil {
		t.Fatalf("UpdateOrgProfile() error = %v", err)
	}
	if saved.CNPJ != "12345678000195" || saved.TradeName != "Escritório" || saved.OnboardingCompletedAt == nil {
		t.Fatalf("saved tenant = %+v, unexpected", saved)
	}
	if saved.Address == nil || saved.Address.CEP != "01311902" || saved.Address.UF != "SP" {
		t.Fatalf("saved address = %+v, want decoded", saved.Address)
	}

	// The columns and the address jsonb actually landed on the row.
	row := readTenantProfile(t, pool, tenantID)
	if row.cnpj == nil || *row.cnpj != "12345678000195" || row.tradeName == nil || *row.tradeName != "Escritório" {
		t.Fatalf("persisted columns = %+v, unexpected", row)
	}
	if row.onboarded == nil {
		t.Fatal("onboarding_completed_at was not stamped")
	}
	var addr identity.Address
	if err := json.Unmarshal(row.address, &addr); err != nil {
		t.Fatalf("address jsonb did not round-trip: %v", err)
	}
	if addr.Cidade != "São Paulo" || addr.UF != "SP" {
		t.Fatalf("address jsonb = %+v, unexpected", addr)
	}

	// Exactly one event, tied to the tenant, unpublished (AC3, same tx).
	if n := countProfileOutbox(t, pool, tenantID); n != 1 {
		t.Fatalf("org_profile_updated outbox rows = %d, want 1", n)
	}
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT payload FROM outbox WHERE type = $1 AND aggregate_id::text = $2`,
		identity.TypeOrgProfileUpdated, tenantID).Scan(&payload); err != nil {
		t.Fatalf("read event payload: %v", err)
	}
	var ev identity.OrgProfileUpdated
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("unmarshal org_profile_updated: %v", err)
	}
	if ev.TenantID != tenantID || ev.CNPJ != "12345678000195" || ev.TradeName != "Escritório" {
		t.Fatalf("event payload = %+v, unexpected", ev)
	}
	if ev.EventID == "" || ev.AggregateID() != tenantID {
		t.Fatalf("event ids = (%q, %q), want a v7 id and the tenant aggregate", ev.EventID, ev.AggregateID())
	}

	// A second PUT (idempotent): the gate must NOT move, other columns update, and
	// a second event is emitted.
	firstOnboardedAt := *row.onboarded
	saved2, err := uc.UpdateOrgProfile(ctx, tenantID, sampleProfile("Escritório Renomeado"))
	if err != nil {
		t.Fatalf("second UpdateOrgProfile() error = %v", err)
	}
	if saved2.OnboardingCompletedAt == nil || !saved2.OnboardingCompletedAt.Equal(firstOnboardedAt) {
		t.Fatalf("onboarding gate moved on replay: %v → %v", firstOnboardedAt, saved2.OnboardingCompletedAt)
	}
	row2 := readTenantProfile(t, pool, tenantID)
	if row2.tradeName == nil || *row2.tradeName != "Escritório Renomeado" {
		t.Fatalf("trade_name not updated on second PUT: %+v", row2)
	}
	if !row2.onboarded.Equal(firstOnboardedAt) {
		t.Fatalf("gate column moved on replay: %v → %v", firstOnboardedAt, *row2.onboarded)
	}
	if n := countProfileOutbox(t, pool, tenantID); n != 2 {
		t.Fatalf("outbox rows after replay = %d, want 2", n)
	}
}

// AC2/AC3: an optional email persists to the tenant.email column, and an address
// left entirely blank is accepted — the profile is saved with no address fields, so
// the org can complete onboarding without an address (it is optional as a whole).
func TestIdentity_UpdateOrgProfile_EmailAndEmptyAddress(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-profile-email", 0)

	profile := identity.OrgProfile{
		CNPJ:      "12345678000195",
		LegalName: "Escritório LTDA",
		TradeName: "Escritório",
		Email:     "contato@escritorio.com.br",
		// Address left as the zero struct: the org saved no address.
	}

	saved, err := newIdentityUC(pool).UpdateOrgProfile(ctx, tenantID, profile)
	if err != nil {
		t.Fatalf("UpdateOrgProfile() error = %v", err)
	}
	if saved.Email != "contato@escritorio.com.br" {
		t.Fatalf("saved email = %q, want the address the PUT carried", saved.Email)
	}
	if saved.OnboardingCompletedAt == nil {
		t.Fatal("onboarding gate not stamped for an address-less profile")
	}
	if saved.Address != nil && *saved.Address != (identity.Address{}) {
		t.Fatalf("saved address = %+v, want no address fields", saved.Address)
	}

	// The email column actually landed on the row.
	var email *string
	if err := pool.QueryRow(ctx, `SELECT email FROM tenant WHERE id = $1`, tenantID).Scan(&email); err != nil {
		t.Fatalf("read tenant email: %v", err)
	}
	if email == nil || *email != "contato@escritorio.com.br" {
		t.Fatalf("persisted email = %v, want contato@escritorio.com.br", email)
	}
}

// AC7 (atomicity): if the publish fails, the whole unit of work rolls back — no
// column is written and no event survives.
func TestIdentity_UpdateOrgProfile_PublishFailRollsBackAll(t *testing.T) {
	pool := newPool(t)
	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-profile-rollback", 0)

	// failingOutbox (defined in acquisition_test.go) satisfies the identity use
	// case's outbox port structurally and aborts the publish inside the tx.
	uc := identity.NewUseCase(
		identity.NewRepository(pool),
		failingOutbox{err: errors.New("outbox unreachable")},
		database.NewUnitOfWork(pool),
	)

	if _, err := uc.UpdateOrgProfile(context.Background(), tenantID, sampleProfile("Escritório")); err == nil {
		t.Fatal("UpdateOrgProfile() error = nil, want the publish failure")
	}

	row := readTenantProfile(t, pool, tenantID)
	if row.cnpj != nil || row.onboarded != nil {
		t.Fatalf("profile columns survived a rolled-back tx: %+v", row)
	}
	if n := countProfileOutbox(t, pool, tenantID); n != 0 {
		t.Fatalf("outbox rows after rollback = %d, want 0", n)
	}
}

// AC7 (scoping): the write touches only the principal's own tenant. Updating A's
// profile leaves B's columns untouched — the WHERE id filter is the barrier (the
// tenant table has no RLS policy of its own).
func TestIdentity_UpdateOrgProfile_ScopedToPrincipalTenant(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-profile-a", 0)
	seedTenant(t, pool, tenantB, "org-profile-b", 0)

	uc := newIdentityUC(pool)
	if _, err := uc.UpdateOrgProfile(ctx, tenantA, sampleProfile("A")); err != nil {
		t.Fatalf("UpdateOrgProfile(A): %v", err)
	}

	if a := readTenantProfile(t, pool, tenantA); a.cnpj == nil {
		t.Fatal("tenant A profile was not written")
	}
	if b := readTenantProfile(t, pool, tenantB); b.cnpj != nil || b.onboarded != nil {
		t.Fatalf("tenant B profile leaked a write from A: %+v", b)
	}
	if n := countProfileOutbox(t, pool, tenantB); n != 0 {
		t.Fatalf("tenant B outbox rows = %d, want 0", n)
	}
}
