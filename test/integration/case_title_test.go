//go:build integration

// Case title (Achado 1) read-model integration tests — prove BuildCaseTitle's inputs
// against a real Postgres: the label written via UseCase.UpdateProcessoManual (the real
// PATCH /v1/processos/:id write path) persists and wins the título on both the processo
// and the intimação read models; the first-captured DEFENDANT (ORDER BY created_at ASC)
// wins the tie-break for a litisconsórcio passivo; and the classe·assunto fallback is
// preserved byte-for-byte when neither a label nor a réu exists yet (zero regression).
package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// setCourtRecordClassSubject fills the class/subject columns seedCourtRecordCNJ leaves
// NULL, so the classe·assunto fallback has real (non-empty) values to assert on.
func setCourtRecordClassSubject(t *testing.T, pool *pgxpool.Pool, recordID, class, subject string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE court_record SET class=$1, subject=$2 WHERE id=$3`, class, subject, recordID); err != nil {
		t.Fatalf("set class/subject: %v", err)
	}
}

// seedPartyAt inserts one party with an EXPLICIT created_at, so a test can control the
// first-captured tie-break deterministically instead of relying on wall-clock ordering
// between two INSERTs a few microseconds apart.
func seedPartyAt(t *testing.T, pool *pgxpool.Pool, tenantID, caseID, role, name string, createdAt time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO party (tenant_id, case_id, role, name, source, created_at)
		 VALUES ($1, $2, $3, $4, 'DJEN', $5)`,
		tenantID, caseID, role, name, createdAt); err != nil {
		t.Fatalf("seed party at %v: %v", createdAt, err)
	}
}

// TestUpdateProcessoManual_Label_PersistsAndDrivesTitle proves the write path end to end
// through the REAL use case (the same one PATCH /v1/processos/:id calls): setting a label
// persists on court_case, the process read model's Title reflects it immediately (winning
// over class/subject), and clearing it (Label="") reverts Title to the fallback — all
// without touching CNJNumber, which stays a separate field throughout.
func TestUpdateProcessoManual_Label_PersistsAndDrivesTitle(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	writeUC := acquisition.NewUseCase(repo, events.NewOutbox(), database.NewUnitOfWork(pool))
	readUC := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-title-label", 0)
	const cnj = "0000777-11.2024.8.26.0100"
	rec, _ := seedCourtRecordCNJ(t, pool, tenantID, cnj)
	setCourtRecordClassSubject(t, pool, rec, "Procedimento Comum Cível", "Cobrança")

	// Before any label: the classe·assunto fallback (zero regression).
	before, err := readUC.Processo(ctx, tenantID, rec)
	if err != nil {
		t.Fatalf("Processo (before label): %v", err)
	}
	if before.Title != "Procedimento Comum Cível · Cobrança" {
		t.Fatalf("Title before label = %q, want the classe·assunto fallback", before.Title)
	}
	if before.Label != nil {
		t.Fatalf("Label before any PATCH = %v, want nil", before.Label)
	}

	// PATCH sets the label.
	const label = "Ação de Cobrança — Cliente ACME"
	lbl := label
	if err := writeUC.UpdateProcessoManual(ctx, tenantID, rec, nil, nil, &lbl); err != nil {
		t.Fatalf("UpdateProcessoManual (set label): %v", err)
	}

	got, err := readUC.Processo(ctx, tenantID, rec)
	if err != nil {
		t.Fatalf("Processo (after label): %v", err)
	}
	if got.Title != label {
		t.Fatalf("Title after label = %q, want %q", got.Title, label)
	}
	if got.Label == nil || *got.Label != label {
		t.Fatalf("Label after PATCH = %v, want %q", got.Label, label)
	}
	if got.CNJNumber != cnj {
		t.Fatalf("CNJNumber = %q, want %q (never folded into Title here)", got.CNJNumber, cnj)
	}

	// PATCH clears the label (Label="", present but empty) — Title reverts to fallback.
	empty := ""
	if err := writeUC.UpdateProcessoManual(ctx, tenantID, rec, nil, nil, &empty); err != nil {
		t.Fatalf("UpdateProcessoManual (clear label): %v", err)
	}
	cleared, err := readUC.Processo(ctx, tenantID, rec)
	if err != nil {
		t.Fatalf("Processo (after clear): %v", err)
	}
	if cleared.Title != "Procedimento Comum Cível · Cobrança" {
		t.Fatalf("Title after clearing label = %q, want the classe·assunto fallback restored", cleared.Title)
	}
	if cleared.Label != nil {
		t.Fatalf("Label after clearing = %v, want nil", cleared.Label)
	}
}

// TestListProcessos_Title_DefendantFallback_FirstCapturedWins proves the réu fallback +
// its tie-break for a litisconsórcio passivo (two réus): the FIRST one ever captured
// (earliest created_at), not alphabetical, wins the título — and the classe·assunto
// sibling process (no label, no réu) keeps the untouched fallback in the SAME list page.
func TestListProcessos_Title_DefendantFallback_FirstCapturedWins(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	readUC := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-title-reu", 0)

	// Processo A: two réus, "ZÉ DA SILVA" captured FIRST (earlier created_at) even though
	// it sorts AFTER "BANCO XYZ" alphabetically — the tie-break must pick the first
	// CAPTURED, not the first by name.
	const cnjA = "0000778-11.2024.8.26.0100"
	recA, caseA := seedCourtRecordCNJ(t, pool, tenantID, cnjA)
	setCourtRecordClassSubject(t, pool, recA, "Execução Fiscal", "Dívida Ativa")
	base := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)
	seedPartyAt(t, pool, tenantID, caseA, "DEFENDANT", "ZÉ DA SILVA", base)
	seedPartyAt(t, pool, tenantID, caseA, "DEFENDANT", "BANCO XYZ S.A.", base.Add(time.Hour))

	// Processo B: no réu, no label — must keep the classe·assunto fallback untouched.
	const cnjB = "0000779-11.2024.8.26.0200"
	recB, _ := seedCourtRecordCNJ(t, pool, tenantID, cnjB)
	setCourtRecordClassSubject(t, pool, recB, "Procedimento Comum Cível", "Indenização")

	byID := map[string]acquisition.ProcessoView{}
	page, err := readUC.Processos(ctx, acquisition.ProcessosQuery{
		TenantID: tenantID, Limit: 20, LastCNJ: firstCNJ, LastID: zeroUUIDlit,
	})
	if err != nil {
		t.Fatalf("Processos: %v", err)
	}
	for _, p := range page.Items {
		byID[p.ID] = p
	}

	gotA, ok := byID[recA]
	if !ok {
		t.Fatalf("processo A (%s) missing from ListProcessos", recA)
	}
	wantA := "ZÉ DA SILVA · " + cnjA
	if gotA.Title != wantA {
		t.Errorf("processo A Title = %q, want %q (first CAPTURED réu, not alphabetical)", gotA.Title, wantA)
	}
	if gotA.CNJNumber != cnjA {
		t.Errorf("processo A CNJNumber = %q, want %q (still a separate field)", gotA.CNJNumber, cnjA)
	}

	gotB, ok := byID[recB]
	if !ok {
		t.Fatalf("processo B (%s) missing from ListProcessos", recB)
	}
	if gotB.Title != "Procedimento Comum Cível · Indenização" {
		t.Errorf("processo B Title = %q, want the untouched classe·assunto fallback", gotB.Title)
	}

	// GetProcesso (the cockpit's own read) mirrors the same título.
	single, err := readUC.Processo(ctx, tenantID, recA)
	if err != nil {
		t.Fatalf("Processo (cockpit): %v", err)
	}
	if single.Title != wantA {
		t.Errorf("GetProcesso Title = %q, want %q (mirrors ListProcessos)", single.Title, wantA)
	}
}

// TestIntimacao_Title_MirrorsProcesso proves the fiação into the intimação read models
// (the painel/detalhe surface): ListIntimacoes and GetIntimacao both carry the SAME
// título as the process they belong to — label wins when set, réu+CNJ otherwise.
func TestIntimacao_Title_MirrorsProcesso(t *testing.T) {
	pool := newPool(t)
	repo := acquisition.NewRepository(pool)
	writeUC := acquisition.NewUseCase(repo, events.NewOutbox(), database.NewUnitOfWork(pool))
	readUC := acquisition.NewReadUseCase(repo)
	ctx := context.Background()

	tenantID := uuid.NewString()
	seedTenant(t, pool, tenantID, "org-title-inti", 0)
	const cnj = "0000780-11.2024.8.26.0100"
	rec, caseID := seedCourtRecordCNJ(t, pool, tenantID, cnj)
	setCourtRecordClassSubject(t, pool, rec, "Procedimento Comum Cível", "Cobrança")
	intiID := seedIntimationReturningID(t, pool, tenantID, caseID, rec)

	// Before any label/réu: classe·assunto fallback on both the list row and the detail.
	listBefore, err := readUC.Intimacoes(ctx, acquisition.IntimacoesQuery{
		TenantID: tenantID, Limit: 20, LastMadeAvailable: maxDateLit, LastID: maxUUIDlit,
	})
	if err != nil {
		t.Fatalf("Intimacoes (before): %v", err)
	}
	var rowBefore *acquisition.IntimacaoView
	for i := range listBefore.Items {
		if listBefore.Items[i].ID == intiID {
			rowBefore = &listBefore.Items[i]
		}
	}
	if rowBefore == nil {
		t.Fatalf("intimação %s missing from ListIntimacoes", intiID)
	}
	if rowBefore.Title != "Procedimento Comum Cível · Cobrança" {
		t.Errorf("list row Title (before) = %q, want the classe·assunto fallback", rowBefore.Title)
	}

	detailBefore, err := readUC.Intimacao(ctx, tenantID, intiID)
	if err != nil {
		t.Fatalf("Intimacao (before): %v", err)
	}
	if detailBefore.Title != "Procedimento Comum Cível · Cobrança" {
		t.Errorf("detail Title (before) = %q, want the classe·assunto fallback", detailBefore.Title)
	}

	// Set the label on the PROCESS (there is no label endpoint on the intimação side).
	const label = "Execução de Título Extrajudicial"
	lbl := label
	if err := writeUC.UpdateProcessoManual(ctx, tenantID, rec, nil, nil, &lbl); err != nil {
		t.Fatalf("UpdateProcessoManual (set label): %v", err)
	}

	listAfter, err := readUC.Intimacoes(ctx, acquisition.IntimacoesQuery{
		TenantID: tenantID, Limit: 20, LastMadeAvailable: maxDateLit, LastID: maxUUIDlit,
	})
	if err != nil {
		t.Fatalf("Intimacoes (after): %v", err)
	}
	var rowAfter *acquisition.IntimacaoView
	for i := range listAfter.Items {
		if listAfter.Items[i].ID == intiID {
			rowAfter = &listAfter.Items[i]
		}
	}
	if rowAfter == nil {
		t.Fatalf("intimação %s missing from ListIntimacoes (after)", intiID)
	}
	if rowAfter.Title != label {
		t.Errorf("list row Title (after label) = %q, want %q", rowAfter.Title, label)
	}
	if rowAfter.CNJNumber != cnj {
		t.Errorf("list row CNJNumber = %q, want %q (still separate)", rowAfter.CNJNumber, cnj)
	}

	detailAfter, err := readUC.Intimacao(ctx, tenantID, intiID)
	if err != nil {
		t.Fatalf("Intimacao (after): %v", err)
	}
	if detailAfter.Title != label {
		t.Errorf("detail Title (after label) = %q, want %q", detailAfter.Title, label)
	}
}
