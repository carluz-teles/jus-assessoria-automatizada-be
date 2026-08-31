package acquisition

import (
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// publisher is the narrow outbox port the use case needs — the producer half of
// the transactional outbox. *events.Outbox satisfies it structurally.
type publisher interface {
	Publish(ctx context.Context, tx database.Tx, ev events.Event) error
	// PublishBatch emits many events in one round-trip — used where a write fans out
	// hundreds of observed events under the advisory lock (a sync window's
	// court_record_observed set), so the lock is held for one insert, not N.
	PublishBatch(ctx context.Context, tx database.Tx, evs []events.Event) error
}

// UseCase carries the acquisition use cases. It depends on the Repository
// interface, the outbox publisher and the UnitOfWork — never on the concrete pg
// implementation. It has exactly two methods: ActivateIntegration and
// ListIntegrations.
type UseCase struct {
	repo    Repository
	outbox  publisher
	uow     database.UnitOfWork
	checker EntitlementChecker
}

// useCaseOption tunes a UseCase at construction — the same option-pattern
// SyncUseCase uses for its own checker (sync.go). It cannot share the name
// WithEntitlementChecker (a Go package cannot declare two funcs with that name
// returning different option types), so this one is named for what it gates.
type useCaseOption func(*UseCase)

// WithActivationEntitlementChecker injects the billing entitlement port
// ActivateIntegration consults, at the edge, before activating a new source —
// the ERD's "entitlements na borda" gate. Without it the use case imposes no
// ceiling (unlimitedEntitlement, the same default sync.go falls back to);
// cmd/api wires the real billing adapter here.
func WithActivationEntitlementChecker(c EntitlementChecker) useCaseOption {
	return func(uc *UseCase) { uc.checker = c }
}

// NewUseCase wires the acquisition use cases to their dependencies.
func NewUseCase(repo Repository, outbox publisher, uow database.UnitOfWork, opts ...useCaseOption) *UseCase {
	uc := &UseCase{repo: repo, outbox: outbox, uow: uow, checker: unlimitedEntitlement{}}
	for _, opt := range opts {
		opt(uc)
	}
	return uc
}

// ActivateIntegration activates the tenant's DJEN watch under the given scope,
// atomically: the integration row is upserted and an integration_activated event
// is written to the outbox in the SAME transaction. DJEN is the only activatable
// source (see ActivateIntegrationRequest) — there is no source selector.
//
// The event fires only when activation changed something: a first activation, a
// changed scope, or a re-activation of a source that was not ACTIVE. Re-posting
// an identical, already-active integration is a no-op event-wise (the row's
// updated_at still advances), so a retry does not spam consumers.
//
// Callers pass validated input (the handler runs Request.Validate first);
// tenantID comes from the verified principal, never the body.
//
// Before any of that, it gates on the tenant's billing entitlement: a tenant
// already at (or above) its active_process_limit is refused outright
// (ErrActivationBlocked, → 403), so it never upserts a row, never publishes the
// event, and never triggers the backfill listener — the ERD's edge-of-the-API
// entitlement gate, distinct from the worker's own per-record gate in sync.go
// (which lets a sync cycle continue past an individual blocked record).
func (uc *UseCase) ActivateIntegration(ctx context.Context, tenantID string, scope Scope) (*Integration, error) {
	if err := uc.checkEntitlement(ctx, tenantID); err != nil {
		return nil, err
	}

	var activated *Integration

	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		before, err := uc.currentIntegration(ctx, tx, tenantID, SourceDJEN)
		if err != nil {
			return err
		}

		after, err := uc.repo.Upsert(ctx, tx, tenantID, SourceDJEN, scope)
		if err != nil {
			return err
		}

		if activationChanged(before, after) {
			if err := uc.outbox.Publish(ctx, tx, newIntegrationActivated(after)); err != nil {
				return err
			}
		}

		activated = after
		return nil
	})
	if err != nil {
		return nil, err
	}
	return activated, nil
}

// ListIntegrations returns the tenant's integrations for a read-only screen. It
// is a plain read model scoped to the tenant; no transaction or event.
func (uc *UseCase) ListIntegrations(ctx context.Context, tenantID string) ([]*Integration, error) {
	return uc.repo.List(ctx, tenantID)
}

// AddWatchedOAB adds one OAB to the tenant's DJEN watch: it resolves-or-creates the
// integration (reusing ActivateIntegration's upsert-and-publish path so a tenant with
// no DJEN integration yet gets one, same entitlement gate) and appends the OAB to its
// scope, deduped. The actual watched_oab row (and, for a brand-new OAB, its onboarding
// backfill) is populated ASYNCHRONOUSLY by the backfill listener reacting to the
// integration_activated event this publishes — same as a full ActivateIntegration call,
// just for a single-OAB delta. The returned WatchedOAB is therefore a synthetic
// just-added view (Name unknown — no capture has run yet), not a re-read of the row.
func (uc *UseCase) AddWatchedOAB(ctx context.Context, tenantID string, oab OABEntry) (*WatchedOAB, error) {
	if err := uc.checkEntitlement(ctx, tenantID); err != nil {
		return nil, err
	}

	canonical := oab.UF + oab.Number
	var added *WatchedOAB

	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		before, err := uc.currentIntegration(ctx, tx, tenantID, SourceDJEN)
		if err != nil {
			return err
		}

		var scope Scope
		if before != nil {
			scope = before.Scope
		}
		if !slices.Contains(scope.OAB, canonical) {
			scope.OAB = append(scope.OAB, canonical)
		}

		after, err := uc.repo.Upsert(ctx, tx, tenantID, SourceDJEN, scope)
		if err != nil {
			return err
		}

		if activationChanged(before, after) {
			if err := uc.outbox.Publish(ctx, tx, newIntegrationActivated(after)); err != nil {
				return err
			}
		}

		added = &WatchedOAB{
			OAB:           canonical,
			OABKey:        oabKey(oab.Number, oab.UF),
			IntegrationID: after.ID,
			Enabled:       true,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return added, nil
}

// ToggleWatchedOAB flips the Termos liga/desliga switch for one already-watched OAB.
// enabled=false disables capture directly (no event — the removal itself needs no
// downstream reaction). enabled=true re-enables via AddOrEnableWatchedOAB; when that
// reports needsCatchUp (the OAB WAS disabled), it publishes WatchedOABReenabled in the
// SAME tx so the backfill listener fires a catch-up scoped to the downtime, never the
// full historical horizon. Toggling an OAB that was never added is ErrWatchedOABNotFound
// (→ 404) — this endpoint only flips an existing watch; AddWatchedOAB creates one.
func (uc *UseCase) ToggleWatchedOAB(ctx context.Context, tenantID, oab string, enabled bool) (*WatchedOAB, error) {
	entry, err := parseOAB(oab)
	if err != nil {
		return nil, err
	}
	key := oabKey(entry.Number, entry.UF)

	var result *WatchedOAB
	err = uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		integ, err := uc.repo.GetBySource(ctx, tx, tenantID, SourceDJEN)
		if errors.Is(err, ErrIntegrationNotFound) {
			return ErrWatchedOABNotFound
		}
		if err != nil {
			return err
		}

		if !enabled {
			row, err := uc.repo.DisableWatchedOAB(ctx, tx, tenantID, integ.ID, key)
			if err != nil {
				return err
			}
			result = &row
			return nil
		}

		row, needsHistory, needsCatchUp, err := uc.repo.AddOrEnableWatchedOAB(ctx, tx, tenantID, integ.ID, key)
		if err != nil {
			return err
		}
		if needsHistory {
			// The row did not exist — AddOrEnableWatchedOAB just inserted it, but this
			// endpoint only TOGGLES an existing watch (AddWatchedOAB is the creator).
			// Returning the error rolls back the tx (uow.Do), undoing that insert.
			return ErrWatchedOABNotFound
		}
		if needsCatchUp && row.CatchUpSince != nil {
			ev := newWatchedOABReenabled(tenantID, integ.ID, integ.Source, key, *row.CatchUpSince)
			if err := uc.outbox.Publish(ctx, tx, ev); err != nil {
				return err
			}
		}
		result = &row
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// AssignResponsible sets (or clears, when assignedUserID is nil) the responsável on the
// court_case behind a court_record, in ONE transaction (UoW → SET LOCAL app.tenant_id, so
// RLS is a second barrier under the explicit tenant filters). The FE addresses the process
// by its court_record :id, so the write first hops record → case (a foreign/unknown id →
// ErrProcessoNotFound, → 404), then — for a non-null assignee — guards that the target user
// is an app_user of this same escritório (else ErrResponsibleNotMember, so a process cannot
// be pinned on someone outside the tenant), and finally writes the assignment. A nil
// assignee is desatribuir and skips the membership guard.
//
// No outbox event here: there is no consumer yet — auditoria/evento (who reassigned, when)
// is a future slice. When that lands, the event publishes in THIS same tx (transactional
// outbox), so the assignment and its audit fact commit together.
//
// tenantID comes from the verified principal, never the body. The caller re-reads the
// ProcessoView afterwards (the read path) so the FE reidrates the header.
func (uc *UseCase) AssignResponsible(ctx context.Context, tenantID, courtRecordID string, assignedUserID *string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		caseID, err := uc.repo.ResolveCaseIDByCourtRecord(ctx, tx, tenantID, courtRecordID)
		if err != nil {
			return err
		}

		if assignedUserID != nil {
			member, err := uc.repo.AppUserInTenant(ctx, tx, tenantID, *assignedUserID)
			if err != nil {
				return err
			}
			if !member {
				return ErrResponsibleNotMember
			}
		}

		if err := uc.repo.AssignCaseResponsible(ctx, tx, tenantID, caseID, assignedUserID); err != nil {
			return err
		}

		// Cascade: the same transaction propagates the case's new responsável to every
		// intimação already anchored under it (via court_record_id → court_record.case_id).
		// Always overwrites — retroactive, no per-intimação opt-out — matching the case-level
		// field of truth.
		_, err = uc.repo.CascadeCaseResponsibleToIntimations(ctx, tx, tenantID, caseID, assignedUserID)
		return err
	})
}

// UpdateProcessoManual grava os campos que o advogado preenche à mão no cockpit — a fase
// (phase_override, que vence a derivada no read model) e o valor da causa (claim_value, sem
// fonte automática). Um argumento nil deixa o campo como está (PATCH parcial). tenantID vem
// do principal, nunca do body.
func (uc *UseCase) UpdateProcessoManual(ctx context.Context, tenantID, courtRecordID string, phaseOverride *string, claimValue *float64) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.repo.UpdateProcessoManualFields(ctx, tx, tenantID, courtRecordID, phaseOverride, claimValue)
	})
}

// BulkAssignResponsible atribui o responsável a vários processos de uma vez. Dois
// modos (mutuamente exclusivos): All=true aplica a TODA a faixa/filtro atual (q —
// mesmos filtros do ListProcessos; inclui os não paginados); senão aplica aos ids
// (court_record ids — mesma granularidade do PUT /processos/:id/responsavel).
// Valida a pertinência do responsável (quando não-nil) antes do write, na mesma tx
// (UoW + RLS) — reusa AppUserInTenant, mesmo guard de AssignResponsible. O repo
// resolve os ids/filtro para os court_case por trás e cascateia pras intimações
// filhas (mesmo padrão de AssignResponsible, só que em lote). Devolve quantos
// processos (court_record) foram afetados.
func (uc *UseCase) BulkAssignResponsible(
	ctx context.Context,
	tenantID string,
	all bool,
	q ProcessosQuery,
	ids []string,
	assignedUserID *string,
) (int64, error) {
	var n int64
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if assignedUserID != nil {
			member, err := uc.repo.AppUserInTenant(ctx, tx, tenantID, *assignedUserID)
			if err != nil {
				return err
			}
			if !member {
				return ErrResponsibleNotMember
			}
		}
		var err error
		if all {
			n, err = uc.repo.BulkAssignResponsibleByFilter(ctx, tx, q, assignedUserID)
		} else {
			n, err = uc.repo.BulkAssignResponsibleByIDs(ctx, tx, tenantID, ids, assignedUserID)
		}
		return err
	})
	return n, err
}

// AssignIntimacaoAssignee sets (or clears, when nil) the single responsável of one
// intimação, in ONE transaction (UoW → SET LOCAL app.tenant_id, RLS as a second barrier
// under the explicit tenant filter). When non-nil, the use case guards that the target
// user is an app_user of the same tenant (reuses AppUserInTenant, same guard as
// AssignResponsible for processos). A nil clears the assignment ("desatribuir"). A miss
// or a foreign tenant's row is ErrIntimationNotFound (→ 404).
//
// No outbox event here: there is no consumer of an assignment fact yet — auditoria/evento
// is a future slice. When that lands, the event publishes in THIS same tx.
// tenantID comes from the verified principal, never the body.
func (uc *UseCase) AssignIntimacaoAssignee(
	ctx context.Context,
	tenantID, intimationID string,
	assigneeUserID *string,
) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if assigneeUserID != nil {
			member, err := uc.repo.AppUserInTenant(ctx, tx, tenantID, *assigneeUserID)
			if err != nil {
				return err
			}
			if !member {
				return ErrResponsibleNotMember
			}
		}
		return uc.repo.AssignIntimacaoAssignee(ctx, tx, tenantID, intimationID, assigneeUserID)
	})
}

// BulkAssignIntimacoes atribui o responsável a várias intimações de uma vez. Dois modos
// (mutuamente exclusivos): All=true aplica a TODA a faixa/filtro atual (q — mesmos
// filtros do ListIntimacoes; inclui os não paginados); senão aplica à lista ids.
// Valida a pertinência do assignee (quando não-nil) antes do write, numa única tx
// (UoW + RLS). Devolve quantas linhas foram afetadas.
func (uc *UseCase) BulkAssignIntimacoes(
	ctx context.Context,
	tenantID string,
	all bool,
	q IntimacoesQuery,
	ids []string,
	assigneeUserID *string,
) (int64, error) {
	var n int64
	err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if assigneeUserID != nil {
			member, err := uc.repo.AppUserInTenant(ctx, tx, tenantID, *assigneeUserID)
			if err != nil {
				return err
			}
			if !member {
				return ErrResponsibleNotMember
			}
		}
		var err error
		if all {
			n, err = uc.repo.BulkAssignIntimacoesByFilter(ctx, tx, q, assigneeUserID)
		} else {
			n, err = uc.repo.BulkAssignIntimacoesByIDs(ctx, tx, tenantID, ids, assigneeUserID)
		}
		return err
	})
	return n, err
}

// ResolveIntimacao / IgnoreIntimacao / ReopenIntimacao are the triagem actions the user
// drives from the inbox: they move ONE intimation's user_status to RESOLVED / IGNORED /
// PENDING, in a single tx (UoW → SET LOCAL app.tenant_id, RLS as a second barrier under
// the explicit tenant filter). They write ONLY user_status — the DJEN cancellation
// `status` is untouched — so the acquisition sync cycle's re-observação upsert (which
// SETs `status`, never `user_status`) can never clobber the user's decision. A miss or a
// foreign tenant's id is the repo's typed ErrIntimationNotFound (→ 404); the handler
// re-reads the detail view afterwards so the FE reidrates the row from the fresh state.
//
// No outbox event here: there is no consumer of a triagem fact yet — an audit/derivation
// event (who resolved, when; a possible prazo effect) is a future slice. When it lands,
// the event publishes in THIS same tx (transactional outbox). tenantID comes from the
// verified principal, never the body.
func (uc *UseCase) ResolveIntimacao(ctx context.Context, tenantID, intimationID string) error {
	return uc.setIntimacaoUserStatus(ctx, tenantID, intimationID, IntimationUserStatusResolved)
}

func (uc *UseCase) IgnoreIntimacao(ctx context.Context, tenantID, intimationID string) error {
	return uc.setIntimacaoUserStatus(ctx, tenantID, intimationID, IntimationUserStatusIgnored)
}

func (uc *UseCase) ReopenIntimacao(ctx context.Context, tenantID, intimationID string) error {
	return uc.setIntimacaoUserStatus(ctx, tenantID, intimationID, IntimationUserStatusPending)
}

// setIntimacaoUserStatus is the shared body of the three triagem actions: one tx that
// writes the target user_status. Kept private — the caller picks the state via the three
// verbs above, so an arbitrary status string never reaches the write.
func (uc *UseCase) setIntimacaoUserStatus(ctx context.Context, tenantID, intimationID, userStatus string) error {
	return uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		return uc.repo.SetIntimationUserStatus(ctx, tx, tenantID, intimationID, userStatus)
	})
}

// checkEntitlement refuses activation when the tenant's ACTIVE court record count
// already meets or exceeds its billing entitlement's active_process_limit. It
// reuses GetReconciliationTotals — the same pool read the reconciliations screen
// already runs to show the tenant's acquired acervo — rather than adding a second
// count query for the same number. Both reads (checker + repo) happen OUTSIDE any
// tx, same as sync.go's applyResult: a small overshoot under a concurrent
// activation is accepted (v0). A tenant with no subscription resolves to limit 0
// upstream (fail-closed), so it is blocked on its very first activation attempt.
func (uc *UseCase) checkEntitlement(ctx context.Context, tenantID string) error {
	limit, err := uc.checker.ActiveProcessLimit(ctx, tenantID)
	if err != nil {
		return err
	}
	totals, err := uc.repo.GetReconciliationTotals(ctx, tenantID)
	if err != nil {
		return err
	}
	if totals.CourtRecords >= limit {
		return ErrActivationBlocked
	}
	return nil
}

// currentIntegration returns the pre-upsert integration for (tenant, source), or
// nil when none exists yet (the first activation). A real lookup error
// propagates; only the typed not-found is folded into nil.
func (uc *UseCase) currentIntegration(ctx context.Context, tx database.Tx, tenantID, source string) (*Integration, error) {
	before, err := uc.repo.GetBySource(ctx, tx, tenantID, source)
	if errors.Is(err, ErrIntegrationNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return before, nil
}

// activationChanged reports whether an activation is meaningful enough to emit
// the event. A first activation (no prior row) always is; otherwise the event
// fires when the source was not already ACTIVE (a re-activation) or its scope
// changed. An identical, already-active re-post is not a change.
func activationChanged(before, after *Integration) bool {
	if before == nil {
		return true
	}
	return before.Status != StatusActive || !scopeEqual(before.Scope, after.Scope)
}

// scopeEqual compares two scopes by value. Order is significant: a reordered OAB
// list counts as a change, which is acceptable — the client controls the order
// and an identical re-post keeps it stable.
func scopeEqual(a, b Scope) bool {
	return slices.Equal(a.OAB, b.OAB) && slices.Equal(a.TaxID, b.TaxID)
}

// newIntegrationActivated builds the event for an activated integration, minting
// a fresh v7 event id (time-ordered) as the aggregate/idempotency key carrier.
func newIntegrationActivated(integ *Integration) IntegrationActivated {
	return IntegrationActivated{
		Base:          events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: integ.ID},
		IntegrationID: integ.ID,
		TenantID:      integ.TenantID,
		Source:        integ.Source,
		Scope:         integ.Scope,
	}
}
