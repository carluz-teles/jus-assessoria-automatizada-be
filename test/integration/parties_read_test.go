//go:build integration

// Parties read-model integration tests — prove GET /processos/:id/partes (the
// ListPartiesByProcesso query) against a real Postgres: the advogados jsonb_agg maps
// back into the counsel views, the parties resolve through court_record → court_case,
// and the read is tenant-scoped (a foreign :id sees nothing). Reference: migration
// 0032_party_schema + the acquisition read model.
package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
)

// seedParty inserts one party under the case and returns its id (owner insert, RLS
// bypassed).
func seedParty(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, role, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO party (tenant_id, case_id, role, name, source)
		 VALUES ($1, $2, $3, $4, 'DJEN') RETURNING id::text`,
		tenantID, caseID, role, name).Scan(&id); err != nil {
		t.Fatalf("seed party: %v", err)
	}
	return id
}

// seedCounsel inserts one advogado under the party (owner insert, RLS bypassed).
func seedCounsel(t *testing.T, pool *pgxpool.Pool, tenantID, partyID, name, oab, uf string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO party_counsel (tenant_id, party_id, name, oab, uf, source)
		 VALUES ($1, $2, $3, $4, $5, 'DJEN')`,
		tenantID, partyID, name, oab, uf); err != nil {
		t.Fatalf("seed party_counsel: %v", err)
	}
}

// TestListPartesByProcesso_BucketsAndCounsels covers the read: the parties resolve via
// the court_record's case, bucket into autor/réu/terceiros, and each carries its
// advogados (aggregated jsonb → counsel views).
func TestListPartesByProcesso_BucketsAndCounsels(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-partes-1", 0)
	recordID, caseID := seedCourtRecordCNJ(t, pool, tenantID, "0000001-11.2024.8.26.0100")

	autor := seedParty(t, pool, tenantID, caseID, "PLAINTIFF", "AUTOR LTDA")
	seedCounsel(t, pool, tenantID, autor, "PEDRO", "119938", "RS")
	seedCounsel(t, pool, tenantID, autor, "ARIVANDO", "41101", "RS")
	seedParty(t, pool, tenantID, caseID, "DEFENDANT", "REU SA")
	seedParty(t, pool, tenantID, caseID, "THIRD_PARTY", "MP")

	rows, err := repo.ListPartesByProcesso(context.Background(), tenantID, recordID)
	if err != nil {
		t.Fatalf("ListPartesByProcesso: %v", err)
	}

	byRole := map[string][]acquisition.PartyRow{}
	for _, r := range rows {
		byRole[r.Role] = append(byRole[r.Role], r)
	}
	if len(byRole["PLAINTIFF"]) != 1 || byRole["PLAINTIFF"][0].Name != "AUTOR LTDA" {
		t.Fatalf("plaintiff rows = %+v, want one AUTOR LTDA", byRole["PLAINTIFF"])
	}
	if got := byRole["PLAINTIFF"][0].Counsels; len(got) != 2 {
		t.Fatalf("autor counsels = %+v, want 2 (jsonb_agg mapped back)", got)
	}
	// Ordered by name inside the jsonb_agg: ARIVANDO before PEDRO.
	if byRole["PLAINTIFF"][0].Counsels[0].OAB != "41101" {
		t.Errorf("first counsel = %+v, want 41101 (ordered by name)", byRole["PLAINTIFF"][0].Counsels[0])
	}
	if len(byRole["DEFENDANT"]) != 1 || len(byRole["DEFENDANT"][0].Counsels) != 0 {
		t.Errorf("réu = %+v, want one with no counsel (empty array, not nil)", byRole["DEFENDANT"])
	}
	if byRole["DEFENDANT"][0].Counsels == nil {
		t.Error("a party with no advogado must have an initialized empty Counsels, not nil")
	}
	if len(byRole["THIRD_PARTY"]) != 1 {
		t.Errorf("terceiros = %+v, want one MP", byRole["THIRD_PARTY"])
	}
}

// TestListPartesByProcesso_TenantScoped proves the read is isolated: a court_record of
// tenant A yields no parties when read under tenant B (barrier 1 — the case is resolved
// only within the caller's tenant).
func TestListPartesByProcesso_TenantScoped(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)

	tenantA := uuid.NewString()
	tenantB := uuid.NewString()
	seedTenant(t, pool, tenantA, "org-partes-a", 0)
	seedTenant(t, pool, tenantB, "org-partes-b", 0)

	recordA, caseA := seedCourtRecordCNJ(t, pool, tenantA, "0000009-99.2024.8.26.0100")
	seedParty(t, pool, tenantA, caseA, "PLAINTIFF", "AUTOR A")

	// Reading tenant A's court_record under tenant B resolves no case → no parties.
	rows, err := repo.ListPartesByProcesso(context.Background(), tenantB, recordA)
	if err != nil {
		t.Fatalf("ListPartesByProcesso (foreign): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("cross-tenant read returned %d parties, want 0", len(rows))
	}

	// Sanity: tenant A sees its own party.
	own, err := repo.ListPartesByProcesso(context.Background(), tenantA, recordA)
	if err != nil {
		t.Fatalf("ListPartesByProcesso (own): %v", err)
	}
	if len(own) != 1 {
		t.Fatalf("own read returned %d parties, want 1", len(own))
	}
}
