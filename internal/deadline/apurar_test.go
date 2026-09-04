package deadline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jusassessoria/platform/lib/apperr"
)

// --- fixtures ---------------------------------------------------------------

// divergentCrossValidation is an undecided, divergent cross_validation for the given
// declared/calculated dates — the state ApurarDivergencia gates on.
func divergentCrossValidation(declared, calculated time.Time) *CrossValidation {
	return &CrossValidation{
		DataDeclarada: declared,
		DataCalculada: calculated,
		DifDias:       10,
		Resultado:     crossValidationDivergente,
	}
}

// apurarRepo primes a mockRepo for the apurar-divergencia path: the load returns the stored
// state, GetCrossValidation returns cv, GetCourtRecordCourt returns court (for the
// ajuste_manual branch), UpdateDeadlineAdjust echoes ids.
func apurarRepo(p adjustParents, cur *DeadlineForAdjust, cv *CrossValidation, court string) *mockRepo {
	return &mockRepo{
		adjustResult:       cur,
		crossValidation:    cv,
		courtRecordCourt:   court,
		updateAdjustID:     p.deadlineID,
		updateAdjustRecord: p.courtRecordID,
	}
}

// --- ApurarDivergencia -------------------------------------------------------

// TestApurarDivergencia_AceitaDeclarado verifies the aceita_declarado branch: end_date is
// overwritten with cross_validation.data_declarada (no recompute), prazo_interno is recomputed
// from that SAME new end_date (never left stale — the regression this slice guards against), the
// decision + selo flip are recorded, and deadline.seal_assigned is emitted with the UNCHANGED
// origem.
func TestApurarDivergencia_AceitaDeclarado(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	declared := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	calculated := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)
	prazoInterno := time.Date(2024, 3, 13, 0, 0, 0, 0, time.UTC) // 2 dias úteis antes do declarado

	stored := storedDeadline(p, start, StatusOpen)
	stored.Origem = OrigemDeclarado
	stored.Selo = SealAApurar
	repo := apurarRepo(p, stored, divergentCrossValidation(declared, calculated), "TJSP")
	outbox := &fakeOutbox{}
	cal := &fakeCalendar{subtractEndDate: prazoInterno}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	res, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Decisao: decisaoAceitaDeclarado,
	})
	if err != nil {
		t.Fatalf("ApurarDivergencia() error = %v", err)
	}
	if !res.EndDate.Equal(declared) {
		t.Errorf("EndDate = %v, want data_declarada %v", res.EndDate, declared)
	}
	if repo.updateEndDateCalls != 1 || !repo.gotUpdateEndDate.Equal(declared) {
		t.Errorf("UpdateDeadlineEndDate calls=%d got=%v, want 1 call with %v", repo.updateEndDateCalls, repo.gotUpdateEndDate, declared)
	}
	// The critical regression this slice fixes: prazo_interno must be recomputed from the SAME
	// new end_date (declared), via the SAME uf/court the branch already resolved — never left
	// pointing at the pre-apuração calculado date.
	if !repo.gotUpdateEndDatePrazoInterno.Equal(prazoInterno) {
		t.Errorf("UpdateDeadlineEndDate prazo_interno = %v, want %v", repo.gotUpdateEndDatePrazoInterno, prazoInterno)
	}
	if cal.subtractCalls != 1 {
		t.Fatalf("SubtractBusinessDays calls = %d, want 1", cal.subtractCalls)
	}
	if !cal.gotSubtractArgs.start.Equal(declared) {
		t.Errorf("SubtractBusinessDays start = %v, want the new end_date %v", cal.gotSubtractArgs.start, declared)
	}
	if cal.gotSubtractArgs.n != internalBufferBusinessDays {
		t.Errorf("SubtractBusinessDays n = %d, want %d", cal.gotSubtractArgs.n, internalBufferBusinessDays)
	}
	if cal.gotSubtractArgs.court != "TJSP" {
		t.Errorf("SubtractBusinessDays court = %q, want %q", cal.gotSubtractArgs.court, "TJSP")
	}
	if repo.updateCVDecisionCalls != 1 || repo.gotUpdateCVDecisionDecisao != string(decisaoAceitaDeclarado) || repo.gotUpdateCVDecisionBy != p.userID {
		t.Errorf("UpdateCrossValidationDecision = calls=%d decisao=%q by=%q, want 1/%q/%q",
			repo.updateCVDecisionCalls, repo.gotUpdateCVDecisionDecisao, repo.gotUpdateCVDecisionBy, decisaoAceitaDeclarado, p.userID)
	}
	if repo.updateSeloCalls != 1 || repo.gotUpdateSelo != SealConfiavel {
		t.Errorf("UpdateDeadlineSelo = calls=%d selo=%q, want 1/%q", repo.updateSeloCalls, repo.gotUpdateSelo, SealConfiavel)
	}
	if repo.gotUpdateSeloConfirmedBy != p.userID {
		t.Errorf("UpdateDeadlineSelo confirmedBy = %q, want the acting user %q", repo.gotUpdateSeloConfirmedBy, p.userID)
	}
	if repo.gotUpdateSeloConfirmedAt.IsZero() {
		t.Error("UpdateDeadlineSelo confirmedAt = zero, want it stamped")
	}
	if repo.gotDeadlineEvent == nil || repo.gotDeadlineEvent.Tipo != "validado" {
		t.Errorf("deadline_event = %+v, want tipo=validado", repo.gotDeadlineEvent)
	}
	// The trilha must show the friendly decisão label, never the raw enum value
	// (aceita_declarado) — a bug caught by visual review of the Trilha screen.
	wantDetalhe := "Divergência apurada: declarado aceito"
	if repo.gotDeadlineEvent == nil || repo.gotDeadlineEvent.Detalhe != wantDetalhe {
		t.Errorf("deadline_event.Detalhe = %q, want %q", repo.gotDeadlineEvent.Detalhe, wantDetalhe)
	}
	if len(outbox.published) != 1 {
		t.Fatalf("published events = %d, want 1 (deadline.seal_assigned)", len(outbox.published))
	}
	sealed, ok := outbox.published[0].(DeadlineSealAssigned)
	if !ok {
		t.Fatalf("published event type = %T, want DeadlineSealAssigned", outbox.published[0])
	}
	if sealed.Origem != string(OrigemDeclarado) {
		t.Errorf("seal_assigned.origem = %q, want UNCHANGED %q", sealed.Origem, OrigemDeclarado)
	}
	if sealed.Seal != string(SealConfiavel) {
		t.Errorf("seal_assigned.seal = %q, want %q", sealed.Seal, SealConfiavel)
	}
}

// TestApurarDivergencia_AceitaCalculado verifies the aceita_calculado branch writes NOTHING to
// end_date (the stored end_date already IS the calculado date) — only the decision + selo flip.
func TestApurarDivergencia_AceitaCalculado(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	declared := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	calculated := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)

	repo := apurarRepo(p, storedDeadline(p, start, StatusOpen), divergentCrossValidation(declared, calculated), "TJSP")
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	res, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Decisao: decisaoAceitaCalculado,
	})
	if err != nil {
		t.Fatalf("ApurarDivergencia() error = %v", err)
	}
	if !res.EndDate.Equal(calculated) {
		t.Errorf("EndDate = %v, want data_calculada %v", res.EndDate, calculated)
	}
	if repo.updateEndDateCalls != 0 {
		t.Errorf("UpdateDeadlineEndDate calls = %d, want 0 (calculado is already the stored end_date)", repo.updateEndDateCalls)
	}
	if repo.updateSeloCalls != 1 || repo.gotUpdateSelo != SealConfiavel {
		t.Errorf("UpdateDeadlineSelo = calls=%d selo=%q, want 1/%q", repo.updateSeloCalls, repo.gotUpdateSelo, SealConfiavel)
	}
	if repo.gotUpdateSeloConfirmedBy != p.userID {
		t.Errorf("UpdateDeadlineSelo confirmedBy = %q, want the acting user %q", repo.gotUpdateSeloConfirmedBy, p.userID)
	}
	wantDetalhe := "Divergência apurada: calculado aceito"
	if repo.gotDeadlineEvent == nil || repo.gotDeadlineEvent.Detalhe != wantDetalhe {
		t.Errorf("deadline_event.Detalhe = %q, want %q", repo.gotDeadlineEvent.Detalhe, wantDetalhe)
	}
}

// TestApurarDivergencia_AjusteManual verifies the ajuste_manual branch: end_date is set DIRECTLY
// to the lawyer-picked data fatal (no recompute from days/counting), prazo_interno is recomputed
// from that SAME new end_date via UpdateDeadlineEndDate (exactly like aceita_declarado), the
// decision + selo flip are recorded, and deadline.seal_assigned is emitted (confiavel).
func TestApurarDivergencia_AjusteManual(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	picked := time.Date(2024, 3, 20, 0, 0, 0, 0, time.UTC)
	declared := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	calculated := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)
	prazoInterno := time.Date(2024, 3, 18, 0, 0, 0, 0, time.UTC)

	stored := storedDeadline(p, start, StatusOpen)
	stored.Selo = SealAApurar
	repo := apurarRepo(p, stored, divergentCrossValidation(declared, calculated), "TJSP")
	cal := &fakeCalendar{subtractEndDate: prazoInterno}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	res, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID,
		Decisao: decisaoAjusteManual, EndDate: &picked,
	})
	if err != nil {
		t.Fatalf("ApurarDivergencia() error = %v", err)
	}
	if !res.EndDate.Equal(picked) {
		t.Errorf("EndDate = %v, want the picked data fatal %v", res.EndDate, picked)
	}
	if repo.updateEndDateCalls != 1 || !repo.gotUpdateEndDate.Equal(picked) {
		t.Errorf("UpdateDeadlineEndDate calls=%d got=%v, want 1 call with %v", repo.updateEndDateCalls, repo.gotUpdateEndDate, picked)
	}
	// The internal buffer must be recomputed from the SAME picked end_date, via the branch's uf/court.
	if !repo.gotUpdateEndDatePrazoInterno.Equal(prazoInterno) {
		t.Errorf("UpdateDeadlineEndDate prazo_interno = %v, want %v", repo.gotUpdateEndDatePrazoInterno, prazoInterno)
	}
	if cal.subtractCalls != 1 || !cal.gotSubtractArgs.start.Equal(picked) {
		t.Errorf("SubtractBusinessDays calls=%d start=%v, want 1 call from the picked end_date %v", cal.subtractCalls, cal.gotSubtractArgs.start, picked)
	}
	if repo.updateAdjustCalls != 0 {
		t.Errorf("UpdateDeadlineAdjust calls = %d, want 0 (ajuste_manual sets end_date directly, no recompute)", repo.updateAdjustCalls)
	}
	if repo.gotUpdateSelo != SealConfiavel {
		t.Errorf("UpdateDeadlineSelo selo = %q, want %q", repo.gotUpdateSelo, SealConfiavel)
	}
	if repo.gotUpdateSeloConfirmedBy != p.userID {
		t.Errorf("UpdateDeadlineSelo confirmedBy = %q, want the acting user %q", repo.gotUpdateSeloConfirmedBy, p.userID)
	}
	wantDetalhe := "Divergência apurada: ajuste manual aplicado"
	if repo.gotDeadlineEvent == nil || repo.gotDeadlineEvent.Detalhe != wantDetalhe {
		t.Errorf("deadline_event.Detalhe = %q, want %q", repo.gotDeadlineEvent.Detalhe, wantDetalhe)
	}
	if len(outbox.published) != 1 {
		t.Fatalf("published events = %d, want 1 (deadline.seal_assigned)", len(outbox.published))
	}
}

// TestApurarDivergencia_AjusteManual_MissingEndDate verifies a nil end_date on ajuste_manual is
// rejected as invalid (the guard inside the branch — belt to the edge validation).
func TestApurarDivergencia_AjusteManual_MissingEndDate(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	declared := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	calculated := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)
	repo := apurarRepo(p, storedDeadline(p, start, StatusOpen), divergentCrossValidation(declared, calculated), "TJSP")
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	_, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Decisao: decisaoAjusteManual,
	})
	if ae, ok := apperr.From(err); !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("error = %v, want a KindInvalid (end_date required)", err)
	}
}

// TestApurarDivergencia_AjusteManual_EndDateBeforeStart verifies a picked end_date on/before the
// termo inicial is rejected as invalid.
func TestApurarDivergencia_AjusteManual_EndDateBeforeStart(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	before := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC) // == start, not after
	declared := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	calculated := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)
	repo := apurarRepo(p, storedDeadline(p, start, StatusOpen), divergentCrossValidation(declared, calculated), "TJSP")
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	_, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID,
		Decisao: decisaoAjusteManual, EndDate: &before,
	})
	if ae, ok := apperr.From(err); !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("error = %v, want a KindInvalid (end_date must be after start)", err)
	}
}

// TestApurarDivergencia_RejectsTerminalStatus verifies MET/CANCELLED prazos refuse apuração
// (CUMPRIDO/BAIXADO_MANUAL — dates are frozen).
func TestApurarDivergencia_RejectsTerminalStatus(t *testing.T) {
	for _, status := range []Status{StatusMet, StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			p := newAdjustParents()
			start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
			repo := apurarRepo(p, storedDeadline(p, start, status), divergentCrossValidation(start, start.AddDate(0, 0, 10)), "TJSP")
			uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

			_, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
				TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Decisao: decisaoAceitaCalculado,
			})
			if !errors.Is(err, ErrDeadlineNotApuravel) {
				t.Errorf("error = %v, want ErrDeadlineNotApuravel", err)
			}
		})
	}
}

// TestApurarDivergencia_RejectsNonDivergent verifies a cross_validation resultado=convergente
// (nothing to apurar) is refused.
func TestApurarDivergencia_RejectsNonDivergent(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	cv := &CrossValidation{DataDeclarada: start, DataCalculada: start, Resultado: crossValidationConvergente}
	repo := apurarRepo(p, storedDeadline(p, start, StatusOpen), cv, "TJSP")
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	_, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Decisao: decisaoAceitaCalculado,
	})
	if !errors.Is(err, ErrDeadlineNotDivergent) {
		t.Errorf("error = %v, want ErrDeadlineNotDivergent", err)
	}
}

// TestApurarDivergencia_Idempotent verifies that a SECOND apuração on an already-resolved
// divergência is refused (idempotency guard), not silently reprocessed.
func TestApurarDivergencia_Idempotent(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	declared := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	calculated := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)
	cv := divergentCrossValidation(declared, calculated)
	repo := apurarRepo(p, storedDeadline(p, start, StatusOpen), cv, "TJSP")
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	cmd := ApurarDivergenciaCommand{TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Decisao: decisaoAceitaCalculado}
	if _, err := uc.ApurarDivergencia(context.Background(), cmd); err != nil {
		t.Fatalf("first ApurarDivergencia() error = %v", err)
	}

	// Simulate the persisted decisão the first call recorded: the SAME cross_validation row now
	// carries a Decisao, so the mock's SECOND read reflects the post-first-call state.
	cv.Decisao = string(decisaoAceitaCalculado)

	if _, err := uc.ApurarDivergencia(context.Background(), cmd); !errors.Is(err, ErrDeadlineNotDivergent) {
		t.Errorf("second ApurarDivergencia() error = %v, want ErrDeadlineNotDivergent (already resolved)", err)
	}
}

// TestApurarDivergencia_ConcurrentRace_UpdateGuardReturnsError proves the concurrency floor a
// pre-check ALONE cannot guarantee: two requests racing on the SAME divergência can both pass
// the in-memory `cv.Decisao == ""` pre-check (read before either writes), so the guard that
// actually prevents a silent overwrite must live in the UPDATE itself (queries/deadline.sql's
// `decisao IS NULL` WHERE clause, mapped by the repository to ErrDeadlineNotDivergent on a
// zero-row UPDATE — see repository.go's UpdateCrossValidationDecision). This test simulates the
// LOSING side of that race: GetCrossValidation still reports undecided (as it would to a
// request that read before the winner committed), but the guarded UPDATE itself reports the
// race (mockRepo.updateCVDecisionErr). ApurarDivergencia must surface ErrDeadlineNotDivergent —
// not swallow it — and must NOT go on to flip selo or publish deadline.seal_assigned, so the
// loser never overwrites the winner's decision.
func TestApurarDivergencia_ConcurrentRace_UpdateGuardReturnsError(t *testing.T) {
	p := newAdjustParents()
	start := time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC)
	declared := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	calculated := time.Date(2024, 3, 25, 0, 0, 0, 0, time.UTC)

	repo := apurarRepo(p, storedDeadline(p, start, StatusOpen), divergentCrossValidation(declared, calculated), "TJSP")
	repo.updateCVDecisionErr = ErrDeadlineNotDivergent
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	_, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Decisao: decisaoAceitaCalculado,
	})
	if !errors.Is(err, ErrDeadlineNotDivergent) {
		t.Errorf("error = %v, want ErrDeadlineNotDivergent (the DB guard caught the race)", err)
	}
	if repo.updateSeloCalls != 0 {
		t.Errorf("UpdateDeadlineSelo calls = %d, want 0 (must not seal after the CV write lost the race)", repo.updateSeloCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 (no seal_assigned on a losing race)", len(outbox.published))
	}
}

// TestApurarDivergencia_InvalidDecisao verifies an out-of-set decisao is rejected BEFORE any
// tx/repo call (validated at the edge of the use case, not inside the transaction).
func TestApurarDivergencia_InvalidDecisao(t *testing.T) {
	p := newAdjustParents()
	repo := &mockRepo{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, uow)

	_, err := uc.ApurarDivergencia(context.Background(), ApurarDivergenciaCommand{
		TenantID: p.tenantID, UserID: p.userID, DeadlineID: p.deadlineID, Decisao: "bogus",
	})
	if err == nil {
		t.Fatal("error = nil, want an invalid-decisao error")
	}
	if len(uow.scopes) != 0 {
		t.Errorf("uow.Do called = %d times, want 0 (validated before opening the tx)", len(uow.scopes))
	}
}
