package draft

// theses_store.go implements the PERSISTED Sugerir Teses use case (C1): it wraps the
// stateless ThesesUseCase.SuggestTheses (RAG+LLM, in-memory) with a draft-scoped
// store so the FE (pecas-v2) gets theses with a stable id/state/position it can
// select and keep across revisits.
//
// Split of concerns vs. tx boundaries:
//   - Generation (RAG+LLM) is external I/O and runs OUTSIDE any tx — it is exactly
//     SuggestTheses, reused verbatim (it already separates its own read phase from
//     the LLM phase). We never hold a DB connection across the LLM call.
//   - Persistence (delete-then-insert) runs in ONE UoW tx: the regenerate is atomic
//     (the old set never coexists with a half-written new set).
//
// POST always regenerates (delete + gera + persiste). The FE only POSTs on the first
// visit (GET came back empty) or on an explicit "Regenerar" — on revisits it GETs the
// persisted rows and does NOT regenerate.

import (
	"context"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/database"
)

// suggestedThesisStore is the narrow persistence port for the persisted theses use
// case — the four suggested_thesis methods, nothing else.
type suggestedThesisStore interface {
	InsertSuggestedThesis(ctx context.Context, tx database.Tx, tenantID string, t *SuggestedThesis) (*SuggestedThesis, error)
	ListSuggestedThesesByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]SuggestedThesis, error)
	UpdateSuggestedThesisState(ctx context.Context, tx database.Tx, tenantID, thesisID, state string) (*SuggestedThesis, error)
	DeleteSuggestedThesesByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) error
	ListSuggestedThesesByIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) ([]SuggestedThesis, error)
	DeleteSuggestedThesesByIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) error
	InsertSuggestedThesisAnchor(ctx context.Context, tx database.Tx, tenantID, thesisID string, a *ThesisAnchor, position int) (*ThesisAnchor, error)
	ListSuggestedThesisAnchorsByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) (map[string][]ThesisAnchor, error)
	ListSuggestedThesisAnchorsByIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) (map[string][]ThesisAnchor, error)
}

// thesisGenerator is the narrow port over ThesesUseCase.SuggestTheses (the stateless
// RAG+LLM generation). Kept as an interface so the store use case is unit-testable
// without an LLM.
type thesisGenerator interface {
	SuggestTheses(ctx context.Context, cmd SuggestThesesCommand) (*SuggestThesesResult, error)
}

// DraftThesesUseCase persists and serves the draft-scoped suggested theses.
type DraftThesesUseCase struct {
	uow   database.UnitOfWork
	store suggestedThesisStore
	gen   thesisGenerator
}

// DraftThesesUseCaseParams groups the construction parameters.
type DraftThesesUseCaseParams struct {
	UoW   database.UnitOfWork
	Store suggestedThesisStore
	Gen   thesisGenerator
}

// NewDraftThesesUseCase wires the persisted-theses use case.
func NewDraftThesesUseCase(p DraftThesesUseCaseParams) *DraftThesesUseCase {
	return &DraftThesesUseCase{uow: p.UoW, store: p.Store, gen: p.Gen}
}

// GenerateDraftTheses regenerates and persists the suggested theses for a draft
// (POST /v1/pecas/:id/theses). It (1) generates OUTSIDE any tx via SuggestTheses
// (RAG+LLM, source_*/grounded already resolved), then (2) in a single UoW tx wipes
// the draft's old theses and inserts the new set with position = order index and the
// initial selection state (pre-select anchored/alta). Returns the persisted list.
func (uc *DraftThesesUseCase) GenerateDraftTheses(ctx context.Context, tenantID, draftID string) ([]SuggestedThesis, error) {
	if draftID == "" {
		return nil, apperr.NewInvalid("GenerateDraftTheses: draft_id é obrigatório")
	}

	// (1) Generation — external I/O, NO tx held across it.
	result, err := uc.gen.SuggestTheses(ctx, SuggestThesesCommand{TenantID: tenantID, DraftID: draftID})
	if err != nil {
		return nil, err
	}

	// (2) Persistence — atomic delete-then-insert in one tx (draft-scoped).
	return uc.persistGenerated(ctx, tenantID, result.Theses, func(tx database.Tx) error {
		return uc.store.DeleteSuggestedThesesByDraft(ctx, tx, tenantID, draftID)
	}, func(t *SuggestedThesis) { t.DraftID = draftID })
}

// GenerateIntimationTheses regenerates and persists the suggested theses for an
// intimation (POST /v1/intimacoes/:id/theses — fluxo da PARTIDA, antes do draft
// existir). Same 2-phase shape as GenerateDraftTheses (generate OUTSIDE tx via
// SuggestTheses in intimation mode, then atomic delete-then-insert), but scoped to
// the intimation. The persisted rows are copied into the draft on promotion.
func (uc *DraftThesesUseCase) GenerateIntimationTheses(ctx context.Context, tenantID, intimationID string) ([]SuggestedThesis, error) {
	if intimationID == "" {
		return nil, apperr.NewInvalid("GenerateIntimationTheses: intimation_id é obrigatório")
	}

	// (1) Generation — external I/O, NO tx held across it. PieceType fica vazio: a
	// partida ainda não escolheu o tipo (o SuggestTheses sintetiza um Draft mínimo
	// só com IntimationID e infere o resto do teor/RAG).
	result, err := uc.gen.SuggestTheses(ctx, SuggestThesesCommand{TenantID: tenantID, IntimationID: intimationID})
	if err != nil {
		return nil, err
	}

	// (2) Persistence — atomic delete-then-insert in one tx (intimation-scoped).
	return uc.persistGenerated(ctx, tenantID, result.Theses, func(tx database.Tx) error {
		return uc.store.DeleteSuggestedThesesByIntimation(ctx, tx, tenantID, intimationID)
	}, func(t *SuggestedThesis) { t.IntimationID = intimationID })
}

// persistGenerated is the shared persistence body of the two Generate* use cases:
// in ONE UoW tx it wipes the old set (via wipe) and inserts the generated theses with
// position = order index and the initial selection state. scope stamps the owning
// draft_id OR intimation_id on each row (mutually exclusive — the caller sets one).
func (uc *DraftThesesUseCase) persistGenerated(
	ctx context.Context,
	tenantID string,
	theses []Thesis,
	wipe func(tx database.Tx) error,
	scope func(*SuggestedThesis),
) ([]SuggestedThesis, error) {
	var persisted []SuggestedThesis
	if err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		if err := wipe(tx); err != nil {
			return err
		}
		persisted = make([]SuggestedThesis, 0, len(theses))
		for i, t := range theses {
			row := &SuggestedThesis{
				Label:            t.Label,
				Confidence:       t.Confidence,
				Reference:        t.Reference,
				Foundation:       t.Foundation,
				Evidence:         t.Evidence,
				SourceRef:        t.SourceRef,
				SourceDocumentID: t.SourceDocumentID,
				SourcePage:       t.SourcePage,
				SourceExcerpt:    t.SourceExcerpt,
				SourceLabel:      t.SourceLabel,
				Grounded:         t.Grounded,
				State:            initialThesisState(t.Grounded, t.Confidence),
				Position:         i, // theses arrive already sorted (sortTheses)
			}
			scope(row)
			inserted, err := uc.store.InsertSuggestedThesis(ctx, tx, tenantID, row)
			if err != nil {
				return err
			}
			// Persist the N anchors of this thesis (multi-âncora, 0094) in the same tx.
			if err := uc.insertAnchors(ctx, tx, tenantID, inserted.ID, t.Anchors); err != nil {
				return err
			}
			inserted.Anchors = t.Anchors
			persisted = append(persisted, *inserted)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return persisted, nil
}

// insertAnchors persists the N anchors of one thesis (multi-âncora, 0094) in the
// caller's tx, position = order index. No-op for an empty slice.
func (uc *DraftThesesUseCase) insertAnchors(ctx context.Context, tx database.Tx, tenantID, thesisID string, anchors []ThesisAnchor) error {
	for i := range anchors {
		if _, err := uc.store.InsertSuggestedThesisAnchor(ctx, tx, tenantID, thesisID, &anchors[i], i); err != nil {
			return err
		}
	}
	return nil
}

// attachDraftAnchors loads the anchors of a draft's theses in ONE query and
// populates each SuggestedThesis.Anchors in place (avoids N+1).
func attachDraftAnchors(ctx context.Context, tx database.Tx, store suggestedThesisStore, tenantID, draftID string, list []SuggestedThesis) error {
	if len(list) == 0 {
		return nil
	}
	byThesis, err := store.ListSuggestedThesisAnchorsByDraft(ctx, tx, tenantID, draftID)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Anchors = byThesis[list[i].ID]
	}
	return nil
}

// attachIntimationAnchors is the intimation-scoped counterpart of attachDraftAnchors.
func attachIntimationAnchors(ctx context.Context, tx database.Tx, store suggestedThesisStore, tenantID, intimationID string, list []SuggestedThesis) error {
	if len(list) == 0 {
		return nil
	}
	byThesis, err := store.ListSuggestedThesisAnchorsByIntimation(ctx, tx, tenantID, intimationID)
	if err != nil {
		return err
	}
	for i := range list {
		list[i].Anchors = byThesis[list[i].ID]
	}
	return nil
}

// ListIntimationTheses returns the persisted theses of an intimation (GET
// /v1/intimacoes/:id/theses — fluxo da partida).
func (uc *DraftThesesUseCase) ListIntimationTheses(ctx context.Context, tenantID, intimationID string) ([]SuggestedThesis, error) {
	var out []SuggestedThesis
	if err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		list, err := uc.store.ListSuggestedThesesByIntimation(ctx, tx, tenantID, intimationID)
		if err != nil {
			return err
		}
		if err := attachIntimationAnchors(ctx, tx, uc.store, tenantID, intimationID, list); err != nil {
			return err
		}
		out = list
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// PromoteIntimationThesesToDraft copies the intimation-scoped theses into the draft
// on partida→construção (called from Create, same tx). The selectedThesisIDs are the
// theses the advogado kept selected on the partida — they land as ThesisStateIncluded;
// all others land as ThesisStateOff. Idempotent/safe: if the draft ALREADY has theses
// (e.g. the advogado revisited the construção and curated the set), it SKIPS — never
// overwrites a curated selection. No draft theses + no intimation theses = no-op.
func (uc *DraftThesesUseCase) PromoteIntimationThesesToDraft(ctx context.Context, tx database.Tx, tenantID, intimationID, draftID string, selectedThesisIDs []string) error {
	return copyIntimationThesesToDraft(ctx, tx, uc.store, tenantID, intimationID, draftID, selectedThesisIDs)
}

// thesisCopyStore is the minimal port copyIntimationThesesToDraft needs — the three
// read/write methods it touches. Both suggestedThesisStore and the full Repository
// satisfy it, so the copy is a single source of truth callable from the Create use
// case (uc.rw) AND from DraftThesesUseCase (uc.store).
type thesisCopyStore interface {
	ListSuggestedThesesByDraft(ctx context.Context, tx database.Tx, tenantID, draftID string) ([]SuggestedThesis, error)
	ListSuggestedThesesByIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) ([]SuggestedThesis, error)
	InsertSuggestedThesis(ctx context.Context, tx database.Tx, tenantID string, t *SuggestedThesis) (*SuggestedThesis, error)
	InsertSuggestedThesisAnchor(ctx context.Context, tx database.Tx, tenantID, thesisID string, a *ThesisAnchor, position int) (*ThesisAnchor, error)
	ListSuggestedThesisAnchorsByIntimation(ctx context.Context, tx database.Tx, tenantID, intimationID string) (map[string][]ThesisAnchor, error)
}

// copyIntimationThesesToDraft is the shared promotion body (see
// PromoteIntimationThesesToDraft / UseCase.promoteIntimationTheses). Idempotent: skips
// when the draft already has theses (never overwrites a curated construção selection);
// no-op when the intimation has none. selectedThesisIDs → ThesisStateIncluded, rest →
// ThesisStateOff.
func copyIntimationThesesToDraft(ctx context.Context, tx database.Tx, store thesisCopyStore, tenantID, intimationID, draftID string, selectedThesisIDs []string) error {
	existing, err := store.ListSuggestedThesesByDraft(ctx, tx, tenantID, draftID)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil // draft já tem teses — não sobrescreve a seleção da construção.
	}

	source, err := store.ListSuggestedThesesByIntimation(ctx, tx, tenantID, intimationID)
	if err != nil {
		return err
	}
	if len(source) == 0 {
		return nil // partida não gerou teses — nada a promover.
	}

	// Load the source theses' anchors once (multi-âncora, 0094) so the copy carries
	// them into the draft-scoped rows.
	anchorsByThesis, err := store.ListSuggestedThesisAnchorsByIntimation(ctx, tx, tenantID, intimationID)
	if err != nil {
		return err
	}

	selected := make(map[string]struct{}, len(selectedThesisIDs))
	for _, id := range selectedThesisIDs {
		selected[id] = struct{}{}
	}

	for i, t := range source {
		state := ThesisStateOff
		if _, ok := selected[t.ID]; ok {
			state = ThesisStateIncluded
		}
		inserted, err := store.InsertSuggestedThesis(ctx, tx, tenantID, &SuggestedThesis{
			DraftID:          draftID,
			Label:            t.Label,
			Confidence:       t.Confidence,
			Reference:        t.Reference,
			Foundation:       t.Foundation,
			Evidence:         t.Evidence,
			SourceRef:        t.SourceRef,
			SourceDocumentID: t.SourceDocumentID,
			SourcePage:       t.SourcePage,
			SourceExcerpt:    t.SourceExcerpt,
			SourceLabel:      t.SourceLabel,
			Grounded:         t.Grounded,
			State:            state,
			Position:         i, // preserva a ordem da partida (já ordenada).
		})
		if err != nil {
			return err
		}
		// Copy the source thesis's anchors into the new draft-scoped thesis.
		for j, a := range anchorsByThesis[t.ID] {
			if _, err := store.InsertSuggestedThesisAnchor(ctx, tx, tenantID, inserted.ID, &a, j); err != nil {
				return err
			}
		}
	}
	return nil
}

// ListDraftTheses returns the persisted theses for a draft (GET /v1/pecas/:id/theses).
func (uc *DraftThesesUseCase) ListDraftTheses(ctx context.Context, tenantID, draftID string) ([]SuggestedThesis, error) {
	var out []SuggestedThesis
	if err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		list, err := uc.store.ListSuggestedThesesByDraft(ctx, tx, tenantID, draftID)
		if err != nil {
			return err
		}
		if err := attachDraftAnchors(ctx, tx, uc.store, tenantID, draftID, list); err != nil {
			return err
		}
		out = list
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateThesisState validates and applies a new selection state to one persisted
// thesis (PATCH /v1/pecas/:id/theses/:thesisId). An unknown state is
// ErrInvalidThesisState; an unknown id is ErrSuggestedThesisNotFound.
func (uc *DraftThesesUseCase) UpdateThesisState(ctx context.Context, tenantID, thesisID, state string) (*SuggestedThesis, error) {
	if !ValidThesisState(state) {
		return nil, ErrInvalidThesisState
	}
	var out *SuggestedThesis
	if err := uc.uow.Do(ctx, tenantID, func(tx database.Tx) error {
		row, err := uc.store.UpdateSuggestedThesisState(ctx, tx, tenantID, thesisID, state)
		if err != nil {
			return err
		}
		out = row
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
