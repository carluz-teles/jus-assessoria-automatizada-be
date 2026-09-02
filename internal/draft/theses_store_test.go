package draft

import (
	"context"
	"errors"
	"testing"

	"github.com/jusassessoria/platform/lib/database"
)

// fakeThesisStore is an in-memory suggestedThesisStore for the persisted-theses
// use case. It records what GenerateDraftTheses inserts (order + state) so tests can
// assert the position/initial-state rules, and whether a Delete preceded the inserts.
type fakeThesisStore struct {
	rows      []SuggestedThesis // draft-scoped inserts land here
	intimRows []SuggestedThesis // intimation-scoped: pre-seeded source for promote/list tests
	deleted   bool
	intimDel  bool
	insertErr error
	updateErr error
	nextID    int
	// Multi-âncora (0094): anchors keyed by thesis id, captured on insert / seeded
	// for intimation-scoped promotion+list tests.
	anchors      map[string][]ThesisAnchor
	intimAnchors map[string][]ThesisAnchor
	segments     map[string][]ThesisSegment
}

func (f *fakeThesisStore) InsertSuggestedThesisAnchor(_ context.Context, _ database.Tx, _, thesisID string, a *ThesisAnchor, _ int) (*ThesisAnchor, error) {
	if f.anchors == nil {
		f.anchors = map[string][]ThesisAnchor{}
	}
	f.anchors[thesisID] = append(f.anchors[thesisID], *a)
	return a, nil
}

func (f *fakeThesisStore) ListSuggestedThesisAnchorsByDraft(_ context.Context, _ database.Tx, _, _ string) (map[string][]ThesisAnchor, error) {
	return f.anchors, nil
}

func (f *fakeThesisStore) ListSuggestedThesisAnchorsByIntimation(_ context.Context, _ database.Tx, _, _ string) (map[string][]ThesisAnchor, error) {
	return f.intimAnchors, nil
}

func (f *fakeThesisStore) ListSuggestedThesisSegmentsByDraft(_ context.Context, _ database.Tx, _, _ string) (map[string][]ThesisSegment, error) {
	return f.segments, nil
}

func (f *fakeThesisStore) InsertSuggestedThesis(_ context.Context, _ database.Tx, _ string, t *SuggestedThesis) (*SuggestedThesis, error) {
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	f.nextID++
	cp := *t
	cp.ID = string(rune('a' + f.nextID - 1))
	// Route by scope so intimation-scoped generates don't pollute the draft slice.
	if t.IntimationID != "" {
		f.intimRows = append(f.intimRows, cp)
	} else {
		f.rows = append(f.rows, cp)
	}
	return &cp, nil
}

func (f *fakeThesisStore) ListSuggestedThesesByDraft(_ context.Context, _ database.Tx, _, _ string) ([]SuggestedThesis, error) {
	return f.rows, nil
}

func (f *fakeThesisStore) ListSuggestedThesesByIntimation(_ context.Context, _ database.Tx, _, _ string) ([]SuggestedThesis, error) {
	return f.intimRows, nil
}

func (f *fakeThesisStore) DeleteSuggestedThesesByIntimation(_ context.Context, _ database.Tx, _, _ string) error {
	f.intimDel = true
	f.intimRows = nil
	return nil
}

func (f *fakeThesisStore) UpdateSuggestedThesisState(_ context.Context, _ database.Tx, _, thesisID, state string) (*SuggestedThesis, error) {
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	for i := range f.rows {
		if f.rows[i].ID == thesisID {
			f.rows[i].State = state
			return &f.rows[i], nil
		}
	}
	return nil, ErrSuggestedThesisNotFound
}

func (f *fakeThesisStore) DeleteSuggestedThesesByDraft(_ context.Context, _ database.Tx, _, _ string) error {
	f.deleted = true
	f.rows = nil
	return nil
}

// fakeThesisGen is a canned thesisGenerator returning a preset in-memory result.
type fakeThesisGen struct {
	result *SuggestThesesResult
	err    error
}

func (f fakeThesisGen) SuggestTheses(context.Context, SuggestThesesCommand) (*SuggestThesesResult, error) {
	return f.result, f.err
}

func TestGenerateDraftTheses_persistsWithPositionAndInitialState(t *testing.T) {
	gen := fakeThesisGen{result: &SuggestThesesResult{Theses: []Thesis{
		{Label: "grounded", Confidence: ThesisConfidenceBaixa, Grounded: true}, // grounded → pending_add
		{Label: "alta", Confidence: ThesisConfidenceAlta, Grounded: false},     // alta → pending_add
		{Label: "weak", Confidence: ThesisConfidenceBaixa, Grounded: false},    // → off
	}}}
	store := &fakeThesisStore{}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: gen})

	got, err := uc.GenerateDraftTheses(context.Background(), "tenant-1", "draft-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !store.deleted {
		t.Fatal("expected regenerate to DELETE before inserting")
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 persisted, got %d", len(got))
	}
	want := []struct {
		pos   int
		state string
	}{
		{0, ThesisStatePendingAdd}, // grounded
		{1, ThesisStatePendingAdd}, // alta
		{2, ThesisStateOff},        // weak
	}
	for i, w := range want {
		if got[i].Position != w.pos {
			t.Errorf("thesis %d: position=%d want %d", i, got[i].Position, w.pos)
		}
		if got[i].State != w.state {
			t.Errorf("thesis %d (%s): state=%q want %q", i, got[i].Label, got[i].State, w.state)
		}
	}
}

// TestGenerateDraftTheses_persistsAnchors covers multi-âncora (0094): each thesis's
// Anchors are persisted in the same tx and reloaded by ListDraftTheses.
func TestGenerateDraftTheses_persistsAnchors(t *testing.T) {
	gen := fakeThesisGen{result: &SuggestThesesResult{Theses: []Thesis{
		{
			Label:      "risco de extinção",
			Confidence: ThesisConfidenceAlta,
			Grounded:   true,
			Anchors: []ThesisAnchor{
				{DocumentID: "doc-cert", Page: 5, Excerpt: "sob pena de extinção", Label: "Certidão · pág. 5", Grounded: true},
				{DocumentID: "doc-ato", Page: 2, Excerpt: "dar andamento", Label: "Ato · pág. 2", Grounded: true},
			},
		},
	}}}
	store := &fakeThesisStore{}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: gen})

	got, err := uc.GenerateDraftTheses(context.Background(), "tenant-1", "draft-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || len(got[0].Anchors) != 2 {
		t.Fatalf("expected 1 thesis with 2 anchors, got %+v", got)
	}
	// Anchors landed in the store keyed by the assigned thesis id.
	if anchors := store.anchors[got[0].ID]; len(anchors) != 2 {
		t.Fatalf("store.anchors[%q] = %d, want 2", got[0].ID, len(anchors))
	}
	// ListDraftTheses reloads the anchors from the store.
	listed, err := uc.ListDraftTheses(context.Background(), "tenant-1", "draft-1")
	if err != nil {
		t.Fatalf("ListDraftTheses error: %v", err)
	}
	if len(listed) != 1 || len(listed[0].Anchors) != 2 {
		t.Fatalf("ListDraftTheses expected 2 anchors, got %+v", listed)
	}
	if listed[0].Anchors[0].DocumentID != "doc-cert" {
		t.Errorf("anchor[0].DocumentID = %q, want doc-cert", listed[0].Anchors[0].DocumentID)
	}
}

func TestGenerateDraftTheses_regenerateWipesOld(t *testing.T) {
	store := &fakeThesisStore{rows: []SuggestedThesis{{ID: "old", Label: "stale"}}}
	gen := fakeThesisGen{result: &SuggestThesesResult{Theses: []Thesis{{Label: "new", Confidence: ThesisConfidenceMedia}}}}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: gen})

	got, err := uc.GenerateDraftTheses(context.Background(), "tenant-1", "draft-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Label != "new" {
		t.Fatalf("expected only the new thesis, got %+v", got)
	}
}

func TestGenerateDraftTheses_generationErrorSkipsPersist(t *testing.T) {
	sentinel := errors.New("llm down")
	store := &fakeThesisStore{}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{
		UoW: fakeUoW{}, Store: store, Gen: fakeThesisGen{err: sentinel},
	})
	if _, err := uc.GenerateDraftTheses(context.Background(), "tenant-1", "draft-1"); !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel, got %v", err)
	}
	if store.deleted {
		t.Fatal("must not touch the store when generation fails")
	}
}

func TestGenerateDraftTheses_requiresDraftID(t *testing.T) {
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: &fakeThesisStore{}, Gen: fakeThesisGen{}})
	if _, err := uc.GenerateDraftTheses(context.Background(), "tenant-1", ""); err == nil {
		t.Fatal("expected error for empty draft_id")
	}
}

func TestGenerateIntimationTheses_persistsIntimationScoped(t *testing.T) {
	gen := fakeThesisGen{result: &SuggestThesesResult{Theses: []Thesis{
		{Label: "alta", Confidence: ThesisConfidenceAlta, Grounded: false}, // → pending_add
		{Label: "weak", Confidence: ThesisConfidenceBaixa, Grounded: false}, // → off
	}}}
	store := &fakeThesisStore{intimRows: []SuggestedThesis{{ID: "old", Label: "stale", IntimationID: "int-1"}}}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: gen})

	got, err := uc.GenerateIntimationTheses(context.Background(), "tenant-1", "int-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !store.intimDel {
		t.Fatal("expected regenerate to DELETE intimation-scoped before inserting")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 persisted, got %d", len(got))
	}
	for _, g := range got {
		if g.IntimationID != "int-1" {
			t.Errorf("thesis %q: intimation_id=%q want int-1", g.Label, g.IntimationID)
		}
		if g.DraftID != "" {
			t.Errorf("thesis %q: draft_id must be empty for intimation scope, got %q", g.Label, g.DraftID)
		}
	}
	if got[0].State != ThesisStatePendingAdd || got[1].State != ThesisStateOff {
		t.Errorf("initial states wrong: %q, %q", got[0].State, got[1].State)
	}
}

func TestGenerateIntimationTheses_requiresIntimationID(t *testing.T) {
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: &fakeThesisStore{}, Gen: fakeThesisGen{}})
	if _, err := uc.GenerateIntimationTheses(context.Background(), "tenant-1", ""); err == nil {
		t.Fatal("expected error for empty intimation_id")
	}
}

func TestListIntimationTheses(t *testing.T) {
	store := &fakeThesisStore{intimRows: []SuggestedThesis{{ID: "a", Label: "x", IntimationID: "int-1"}}}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: fakeThesisGen{}})
	got, err := uc.ListIntimationTheses(context.Background(), "tenant-1", "int-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("expected the intimation's theses, got %+v", got)
	}
}

func TestPromoteIntimationThesesToDraft_mapsSelectionToState(t *testing.T) {
	store := &fakeThesisStore{intimRows: []SuggestedThesis{
		{ID: "a", Label: "keep", IntimationID: "int-1", State: ThesisStatePendingAdd},
		{ID: "b", Label: "drop", IntimationID: "int-1", State: ThesisStateOff},
		{ID: "c", Label: "also-keep", IntimationID: "int-1", State: ThesisStateOff},
	}}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: fakeThesisGen{}})

	err := uc.PromoteIntimationThesesToDraft(context.Background(), nil, "tenant-1", "int-1", "draft-1", []string{"a", "c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.rows) != 3 {
		t.Fatalf("expected 3 copied into the draft, got %d", len(store.rows))
	}
	byLabel := map[string]SuggestedThesis{}
	for _, r := range store.rows {
		byLabel[r.Label] = r
		if r.DraftID != "draft-1" {
			t.Errorf("copied thesis %q: draft_id=%q want draft-1", r.Label, r.DraftID)
		}
		if r.IntimationID != "" {
			t.Errorf("copied thesis %q must not keep intimation_id, got %q", r.Label, r.IntimationID)
		}
	}
	if byLabel["keep"].State != ThesisStateIncluded || byLabel["also-keep"].State != ThesisStateIncluded {
		t.Error("selected theses must land as included")
	}
	if byLabel["drop"].State != ThesisStateOff {
		t.Error("unselected thesis must land as off")
	}
}

func TestPromoteIntimationThesesToDraft_skipsWhenDraftHasTheses(t *testing.T) {
	store := &fakeThesisStore{
		rows:      []SuggestedThesis{{ID: "curated", Label: "curated", DraftID: "draft-1"}},
		intimRows: []SuggestedThesis{{ID: "a", Label: "src", IntimationID: "int-1"}},
	}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: fakeThesisGen{}})
	if err := uc.PromoteIntimationThesesToDraft(context.Background(), nil, "tenant-1", "int-1", "draft-1", []string{"a"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(store.rows) != 1 || store.rows[0].ID != "curated" {
		t.Fatalf("must not overwrite a curated draft selection, got %+v", store.rows)
	}
}

func TestUpdateThesisState_valid(t *testing.T) {
	store := &fakeThesisStore{rows: []SuggestedThesis{{ID: "a", State: ThesisStateOff}}}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: fakeThesisGen{}})
	got, err := uc.UpdateThesisState(context.Background(), "tenant-1", "a", ThesisStateIncluded)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.State != ThesisStateIncluded {
		t.Fatalf("state=%q want included", got.State)
	}
}

func TestUpdateThesisState_invalidEnum(t *testing.T) {
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: &fakeThesisStore{}, Gen: fakeThesisGen{}})
	if _, err := uc.UpdateThesisState(context.Background(), "tenant-1", "a", "bogus"); !errors.Is(err, ErrInvalidThesisState) {
		t.Fatalf("expected ErrInvalidThesisState, got %v", err)
	}
}

func TestUpdateThesisState_notFound(t *testing.T) {
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: &fakeThesisStore{}, Gen: fakeThesisGen{}})
	if _, err := uc.UpdateThesisState(context.Background(), "tenant-1", "ghost", ThesisStateOff); !errors.Is(err, ErrSuggestedThesisNotFound) {
		t.Fatalf("expected ErrSuggestedThesisNotFound, got %v", err)
	}
}

func TestListDraftTheses_passthrough(t *testing.T) {
	store := &fakeThesisStore{rows: []SuggestedThesis{{ID: "a"}, {ID: "b"}}}
	uc := NewDraftThesesUseCase(DraftThesesUseCaseParams{UoW: fakeUoW{}, Store: store, Gen: fakeThesisGen{}})
	got, err := uc.ListDraftTheses(context.Background(), "tenant-1", "draft-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
}
