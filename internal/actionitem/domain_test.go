package actionitem

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// fakeUOW is a no-op unit of work: it runs fn with a nil tx (the mocked repo never
// touches it) and records the RLS scope the use case asked for.
type fakeUOW struct {
	scope string
	err   error
}

func (u *fakeUOW) Do(_ context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scope = tenantID
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

// recordingOutbox captures what a use case publishes.
type recordingOutbox struct {
	published []events.Event
	err       error
}

func (r *recordingOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	if r.err != nil {
		return r.err
	}
	r.published = append(r.published, ev)
	return nil
}

// fakeDedup is the deduper port fake: SeenOrMark returns a fixed answer, defaulting to
// "never seen before" (seen=false) unless configured otherwise.
type fakeDedup struct {
	seen  bool
	err   error
	calls int
}

func (d *fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, _, _ string) (bool, error) {
	d.calls++
	return d.seen, d.err
}

// mockRepo is a hand-rolled Repository backed by an in-memory map, enough to drive the
// materialize/confirmar/descartar flows without a real database.
type mockRepo struct {
	items map[string]*ActionItem

	insertCalls int
	deleteCalls int
	existsCalls int

	confirmErr error
	discardErr error

	linkTaskErr   error
	linkTaskCalls int

	hasFiledDraft      bool
	hasFiledDraftErr   error
	hasFiledDraftCalls int

	reclassifyErr   error
	reclassifyCalls int
}

func newMockRepo() *mockRepo { return &mockRepo{items: map[string]*ActionItem{}} }

func (m *mockRepo) seed(a *ActionItem) *ActionItem {
	cp := *a
	m.items[cp.ID] = &cp
	return &cp
}

func (m *mockRepo) InsertActionItem(_ context.Context, _ database.Tx, a *ActionItem) (*ActionItem, error) {
	m.insertCalls++
	cp := *a
	m.items[cp.ID] = &cp
	out := cp
	return &out, nil
}

func (m *mockRepo) GetActionItem(_ context.Context, _ database.Tx, tenantID, id string) (*ActionItem, error) {
	item, ok := m.items[id]
	if !ok || item.TenantID != tenantID {
		return nil, ErrActionItemNotFound
	}
	cp := *item
	return &cp, nil
}

func (m *mockRepo) ConfirmActionItem(_ context.Context, _ database.Tx, tenantID, id string) (*ActionItem, error) {
	if m.confirmErr != nil {
		return nil, m.confirmErr
	}
	item, ok := m.items[id]
	if !ok || item.TenantID != tenantID || item.TipoStatus != TipoStatusAConfirmar || item.Status == StatusDiscarded {
		return nil, ErrActionItemConflict
	}
	item.TipoStatus = TipoStatusConfiavel
	cp := *item
	return &cp, nil
}

func (m *mockRepo) DiscardActionItem(_ context.Context, _ database.Tx, tenantID, id string) (*ActionItem, error) {
	if m.discardErr != nil {
		return nil, m.discardErr
	}
	item, ok := m.items[id]
	if !ok || item.TenantID != tenantID || item.Status == StatusDiscarded {
		return nil, ErrActionItemConflict
	}
	item.Status = StatusDiscarded
	cp := *item
	return &cp, nil
}

func (m *mockRepo) DeleteReplaceableActionItems(_ context.Context, _ database.Tx, tenantID, intimationID string) error {
	m.deleteCalls++
	for id, item := range m.items {
		if item.TenantID == tenantID && item.IntimationID == intimationID &&
			item.TaskID == "" && item.Status == StatusSuggested && item.TipoStatus == TipoStatusAConfirmar {
			delete(m.items, id)
		}
	}
	return nil
}

// LinkTask writes task_id + status=CONFIRMED on the seeded item, guarded by task_id=="" —
// mirrors the real repo's task_id IS NULL guard. A miss, a foreign tenant, or an already-
// linked item all report ErrActionItemNotFound, letting OnTaskCreated's no-op path be tested
// without a real DB.
func (m *mockRepo) LinkTask(_ context.Context, _ database.Tx, tenantID, id, taskID string) (*ActionItem, error) {
	m.linkTaskCalls++
	if m.linkTaskErr != nil {
		return nil, m.linkTaskErr
	}
	item, ok := m.items[id]
	if !ok || item.TenantID != tenantID || item.TaskID != "" {
		return nil, ErrActionItemNotFound
	}
	item.TaskID = taskID
	item.Status = StatusConfirmed
	cp := *item
	return &cp, nil
}

// HasFiledDraftForActionItem returns the fixed m.hasFiledDraft answer (or m.hasFiledDraftErr
// when set) — the test seeds whichever scenario the guard needs, no real draft/task tables
// to fake.
func (m *mockRepo) HasFiledDraftForActionItem(_ context.Context, _ database.Tx, _, _ string) (bool, error) {
	m.hasFiledDraftCalls++
	if m.hasFiledDraftErr != nil {
		return false, m.hasFiledDraftErr
	}
	return m.hasFiledDraft, nil
}

// ReclassifyActionItem mirrors ConfirmActionItem/DiscardActionItem's guard shape: a
// concurrent descartar (status became DISCARDED between the use case's pre-read and this
// call) yields ErrActionItemConflict, same as the real repo's guarded UPDATE.
func (m *mockRepo) ReclassifyActionItem(_ context.Context, _ database.Tx, tenantID, id, pieceProfileKey, tipo string) (*ActionItem, error) {
	m.reclassifyCalls++
	if m.reclassifyErr != nil {
		return nil, m.reclassifyErr
	}
	item, ok := m.items[id]
	if !ok || item.TenantID != tenantID || item.Status == StatusDiscarded {
		return nil, ErrActionItemConflict
	}
	item.PieceProfileKey = pieceProfileKey
	item.Tipo = tipo
	item.TipoOrigem = TipoOrigemManual
	item.TipoStatus = TipoStatusConfiavel
	item.GeraPeca = true
	item.Confianca = nil
	cp := *item
	return &cp, nil
}

func (m *mockRepo) ExistsActionItemByTipo(_ context.Context, _ database.Tx, tenantID, intimationID, tipo string, tipoOrigem TipoOrigem) (bool, error) {
	m.existsCalls++
	for _, item := range m.items {
		if item.TenantID == tenantID && item.IntimationID == intimationID && item.Tipo == tipo && item.TipoOrigem == tipoOrigem {
			return true, nil
		}
	}
	return false, nil
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// --- OnIntimationAnalyzed --------------------------------------------------

func TestOnIntimationAnalyzed_MaterializesCandidates(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	outbox := &recordingOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, outbox, &fakeDedup{}, uow, WithClock(fixedClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))))

	profileKey := "contestacao"
	confianca := 0.7
	ev := IntimationAnalyzed{
		Base:          events.Base{EventID: uuid.NewString()},
		TenantID:      "t1",
		IntimationID:  "i1",
		CourtRecordID: "cr1",
		DeadlineID:    "d1",
		Providencias: []ProvidenciaCandidate{
			{Tipo: TipoContestar, GeraPeca: true, PieceProfileKey: &profileKey, Declarado: true},
			{Tipo: TipoManifestar, GeraPeca: false, Declarado: false, Confianca: &confianca},
		},
	}

	if err := uc.OnIntimationAnalyzed(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}
	if uow.scope != "t1" {
		t.Errorf("uow scope = %q, want t1", uow.scope)
	}
	if repo.insertCalls != 2 {
		t.Fatalf("insert calls = %d, want 2", repo.insertCalls)
	}

	var declarado, inferido *ActionItem
	for _, item := range repo.items {
		switch item.Tipo {
		case TipoContestar:
			declarado = item
		case TipoManifestar:
			inferido = item
		}
	}
	if declarado == nil || declarado.TipoOrigem != TipoOrigemDeclarado || declarado.TipoStatus != TipoStatusConfiavel {
		t.Errorf("declarado item = %+v, want tipo_origem=declarado tipo_status=confiavel", declarado)
	}
	if !declarado.GeraPeca || declarado.PieceProfileKey != "contestacao" {
		t.Errorf("declarado item peça = {%v %q}, want {true contestacao}", declarado.GeraPeca, declarado.PieceProfileKey)
	}
	if declarado.CourtRecordID != "cr1" || declarado.DeadlineID != "d1" {
		t.Errorf("declarado item context = {%q %q}, want {cr1 d1}", declarado.CourtRecordID, declarado.DeadlineID)
	}
	if inferido == nil || inferido.TipoOrigem != TipoOrigemIA || inferido.TipoStatus != TipoStatusAConfirmar {
		t.Errorf("inferido item = %+v, want tipo_origem=ia tipo_status=a_confirmar", inferido)
	}
	if inferido.Confianca == nil || *inferido.Confianca != 0.7 {
		t.Errorf("inferido confianca = %v, want 0.7", inferido.Confianca)
	}

	// Only the confiável (declarado) item is born ready — it alone emits actionitem.created.
	if len(outbox.published) != 1 {
		t.Fatalf("published = %d, want 1", len(outbox.published))
	}
	created, ok := outbox.published[0].(ActionItemCreated)
	if !ok {
		t.Fatalf("published[0] = %T, want ActionItemCreated", outbox.published[0])
	}
	if created.ActionItemID != declarado.ID {
		t.Errorf("created.ActionItemID = %q, want %q", created.ActionItemID, declarado.ID)
	}
}

func TestOnIntimationAnalyzed_Dedup(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	outbox := &recordingOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, outbox, dedup, &fakeUOW{})

	ev := IntimationAnalyzed{
		Base:         events.Base{EventID: uuid.NewString()},
		TenantID:     "t1",
		IntimationID: "i1",
		Providencias: []ProvidenciaCandidate{{Tipo: TipoContestar, GeraPeca: false, Declarado: true}},
	}

	if err := uc.OnIntimationAnalyzed(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}
	if dedup.calls != 1 {
		t.Fatalf("dedup calls = %d, want 1", dedup.calls)
	}
	if repo.insertCalls != 0 {
		t.Errorf("insert calls = %d, want 0 (a replay must not materialize twice)", repo.insertCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published = %d, want 0", len(outbox.published))
	}
}

// TestOnIntimationAnalyzed_GuardAditivo is the guard's own acceptance criterion: a
// re-analysis NEVER touches a task-bound item nor a confiável one already committed, and
// never duplicates a confiável candidate the DELETE didn't clear — but freely replaces the
// SUGGESTED+a_confirmar+no-task subset.
func TestOnIntimationAnalyzed_GuardAditivo(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	committed := repo.seed(&ActionItem{
		ID: "committed", TenantID: "t1", IntimationID: "i1", Tipo: TipoRecorrer,
		TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusConfiavel, Status: StatusSuggested, TaskID: "task-1",
	})
	replaceable := repo.seed(&ActionItem{
		ID: "replaceable", TenantID: "t1", IntimationID: "i1", Tipo: TipoManifestar,
		TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar, Status: StatusSuggested,
	})
	declaredAlready := repo.seed(&ActionItem{
		ID: "declared", TenantID: "t1", IntimationID: "i1", Tipo: TipoContestar,
		TipoOrigem: TipoOrigemDeclarado, TipoStatus: TipoStatusConfiavel, Status: StatusSuggested,
	})

	outbox := &recordingOutbox{}
	uc := NewUseCase(repo, outbox, &fakeDedup{}, &fakeUOW{})

	ev := IntimationAnalyzed{
		Base:         events.Base{EventID: uuid.NewString()},
		TenantID:     "t1",
		IntimationID: "i1",
		Providencias: []ProvidenciaCandidate{
			// Re-proposes the SAME declarado tipo already committed — must be skipped (dedup),
			// not duplicated.
			{Tipo: TipoContestar, GeraPeca: false, Declarado: true},
			// A brand-new inferred candidate — inserted fresh.
			{Tipo: TipoCumprir, GeraPeca: false, Declarado: false},
		},
	}
	if err := uc.OnIntimationAnalyzed(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}

	if _, ok := repo.items[committed.ID]; !ok {
		t.Error("task-bound item was removed, want it untouched (guard aditivo)")
	}
	if _, ok := repo.items[replaceable.ID]; ok {
		t.Error("SUGGESTED+a_confirmar+no-task item survived, want it replaced")
	}
	if _, ok := repo.items[declaredAlready.ID]; !ok {
		t.Error("already-committed declarado item was removed, want it untouched")
	}

	// Exactly one NEW item (the cumprir candidate) — the repeated contestar was deduped.
	var newItems int
	for id := range repo.items {
		if id != committed.ID && id != declaredAlready.ID {
			newItems++
		}
	}
	if newItems != 1 {
		t.Errorf("new items after materialization = %d, want 1 (dedup must skip the repeated declarado)", newItems)
	}
	if repo.insertCalls != 1 {
		t.Errorf("insert calls = %d, want 1 (the repeated declarado is skipped before insert)", repo.insertCalls)
	}
}

func TestOnIntimationAnalyzed_SanitizesInvalidCandidate(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

	garbage := "peca-que-nao-existe"
	ev := IntimationAnalyzed{
		Base:         events.Base{EventID: uuid.NewString()},
		TenantID:     "t1",
		IntimationID: "i1",
		Providencias: []ProvidenciaCandidate{
			{Tipo: "chutar-uma-moeda", GeraPeca: true, PieceProfileKey: &garbage, Declarado: false},
		},
	}
	if err := uc.OnIntimationAnalyzed(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}
	if repo.insertCalls != 1 {
		t.Fatalf("insert calls = %d, want 1 (degrade, never fail the event)", repo.insertCalls)
	}
	for _, item := range repo.items {
		if item.Tipo != TipoCiencia || item.GeraPeca || item.PieceProfileKey != "" {
			t.Errorf("sanitized item = %+v, want {tipo:ciencia gera_peca:false piece_profile_key:\"\"}", item)
		}
	}
}

// --- Confirmar ---------------------------------------------------------------

func TestConfirmar(t *testing.T) {
	t.Parallel()

	t.Run("a_confirmar becomes confiavel and emits", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar, Status: StatusSuggested})
		outbox := &recordingOutbox{}
		uc := NewUseCase(repo, outbox, &fakeDedup{}, &fakeUOW{})

		item, err := uc.Confirmar(context.Background(), "t1", "a1")
		if err != nil {
			t.Fatalf("Confirmar() error = %v", err)
		}
		if item.TipoStatus != TipoStatusConfiavel {
			t.Errorf("tipo_status = %q, want confiavel", item.TipoStatus)
		}
		if len(outbox.published) != 1 {
			t.Fatalf("published = %d, want 1", len(outbox.published))
		}
		if _, ok := outbox.published[0].(ActionItemConfirmed); !ok {
			t.Errorf("published[0] = %T, want ActionItemConfirmed", outbox.published[0])
		}
	})

	t.Run("already confiavel is an idempotent no-op", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemDeclarado, TipoStatus: TipoStatusConfiavel, Status: StatusSuggested})
		outbox := &recordingOutbox{}
		uc := NewUseCase(repo, outbox, &fakeDedup{}, &fakeUOW{})

		item, err := uc.Confirmar(context.Background(), "t1", "a1")
		if err != nil {
			t.Fatalf("Confirmar() error = %v", err)
		}
		if item.TipoStatus != TipoStatusConfiavel {
			t.Errorf("tipo_status = %q, want confiavel (unchanged)", item.TipoStatus)
		}
		if len(outbox.published) != 0 {
			t.Errorf("published = %d, want 0 (no re-emit on idempotent no-op)", len(outbox.published))
		}
	})

	t.Run("discarded item cannot be confirmed", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar, Status: StatusDiscarded})
		uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

		_, err := uc.Confirmar(context.Background(), "t1", "a1")
		if !errors.Is(err, ErrActionItemDiscarded) {
			t.Fatalf("err = %v, want ErrActionItemDiscarded", err)
		}
	})

	t.Run("missing item is a typed 404", func(t *testing.T) {
		t.Parallel()
		uc := NewUseCase(newMockRepo(), &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

		_, err := uc.Confirmar(context.Background(), "t1", "ghost")
		if !errors.Is(err, ErrActionItemNotFound) {
			t.Fatalf("err = %v, want ErrActionItemNotFound", err)
		}
	})
}

// --- Descartar ---------------------------------------------------------------

func TestDescartar(t *testing.T) {
	t.Parallel()

	t.Run("suggested becomes discarded and emits", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar, Status: StatusSuggested})
		outbox := &recordingOutbox{}
		uc := NewUseCase(repo, outbox, &fakeDedup{}, &fakeUOW{})

		item, err := uc.Descartar(context.Background(), "t1", "a1")
		if err != nil {
			t.Fatalf("Descartar() error = %v", err)
		}
		if item.Status != StatusDiscarded {
			t.Errorf("status = %q, want DISCARDED", item.Status)
		}
		if len(outbox.published) != 1 {
			t.Fatalf("published = %d, want 1", len(outbox.published))
		}
		if _, ok := outbox.published[0].(ActionItemDiscarded); !ok {
			t.Errorf("published[0] = %T, want ActionItemDiscarded", outbox.published[0])
		}
	})

	t.Run("already discarded is an idempotent no-op", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", Status: StatusDiscarded})
		outbox := &recordingOutbox{}
		uc := NewUseCase(repo, outbox, &fakeDedup{}, &fakeUOW{})

		item, err := uc.Descartar(context.Background(), "t1", "a1")
		if err != nil {
			t.Fatalf("Descartar() error = %v", err)
		}
		if item.Status != StatusDiscarded {
			t.Errorf("status = %q, want DISCARDED (unchanged)", item.Status)
		}
		if len(outbox.published) != 0 {
			t.Errorf("published = %d, want 0 (no re-emit on idempotent no-op)", len(outbox.published))
		}
	})

	t.Run("missing item is a typed 404", func(t *testing.T) {
		t.Parallel()
		uc := NewUseCase(newMockRepo(), &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

		_, err := uc.Descartar(context.Background(), "t1", "ghost")
		if !errors.Is(err, ErrActionItemNotFound) {
			t.Fatalf("err = %v, want ErrActionItemNotFound", err)
		}
	})
}

// --- Reclassificar (fatia 5, docs §7 questão 4) -----------------------------------------

func TestReclassificar(t *testing.T) {
	t.Parallel()

	t.Run("overrides tipo/piece_profile_key and emits", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		// Confianca starts NON-nil (0.8): a trivial nil-seeded item would make the
		// "Confianca reset" assertion below pass vacuously (nil stays nil) without ever
		// exercising the reset. Seeding a real score here is what makes this a meaningful
		// proof that Reclassificar's result carries confianca=nil, not just an artifact of
		// the zero value. The REAL reset (migration 0078's action_item_check1: tipo_origem =
		// 'ia' OR confianca IS NULL) only bites at the SQL layer — proven against a real
		// Postgres by TestReclassify_IAOrigemWithConfianca_ResetsConfiancaAndSatisfiesCheck
		// in test/integration/action_item_reclassify_test.go.
		confianca := 0.8
		repo.seed(&ActionItem{
			ID: "a1", TenantID: "t1", Tipo: TipoManifestar, GeraPeca: false,
			TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar, Status: StatusSuggested,
			Confianca: &confianca,
		})
		outbox := &recordingOutbox{}
		uc := NewUseCase(repo, outbox, &fakeDedup{}, &fakeUOW{})

		item, err := uc.Reclassificar(context.Background(), "t1", "a1", "contestacao", TipoContestar)
		if err != nil {
			t.Fatalf("Reclassificar() error = %v", err)
		}
		if item.PieceProfileKey != "contestacao" || item.Tipo != TipoContestar {
			t.Errorf("item = {%q %q}, want {contestacao %q}", item.PieceProfileKey, item.Tipo, TipoContestar)
		}
		if item.TipoOrigem != TipoOrigemManual || item.TipoStatus != TipoStatusConfiavel {
			t.Errorf("item = {%q %q}, want {manual confiavel}", item.TipoOrigem, item.TipoStatus)
		}
		if !item.GeraPeca {
			t.Error("item.GeraPeca = false, want true (forced)")
		}
		if item.Confianca != nil {
			t.Errorf("item.Confianca = %v, want nil (reset from the seeded 0.8)", *item.Confianca)
		}
		if len(outbox.published) != 1 {
			t.Fatalf("published = %d, want 1", len(outbox.published))
		}
		reclassified, ok := outbox.published[0].(ActionItemReclassified)
		if !ok {
			t.Fatalf("published[0] = %T, want ActionItemReclassified", outbox.published[0])
		}
		if reclassified.ActionItemID != "a1" {
			t.Errorf("reclassified.ActionItemID = %q, want a1", reclassified.ActionItemID)
		}
	})

	t.Run("unknown piece_profile_key is rejected before touching the repo", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar, Status: StatusSuggested})
		uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

		_, err := uc.Reclassificar(context.Background(), "t1", "a1", "peca-que-nao-existe", TipoContestar)
		if !errors.Is(err, ErrUnknownPieceProfileKey) {
			t.Fatalf("err = %v, want ErrUnknownPieceProfileKey", err)
		}
		if repo.reclassifyCalls != 0 {
			t.Errorf("reclassify calls = %d, want 0 (rejected before the tx)", repo.reclassifyCalls)
		}
	})

	t.Run("invalid tipo is rejected before touching the repo", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar, Status: StatusSuggested})
		uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

		_, err := uc.Reclassificar(context.Background(), "t1", "a1", "contestacao", "chutar-uma-moeda")
		if !errors.Is(err, ErrInvalidTipoReclassify) {
			t.Fatalf("err = %v, want ErrInvalidTipoReclassify", err)
		}
		if repo.reclassifyCalls != 0 {
			t.Errorf("reclassify calls = %d, want 0 (rejected before the tx)", repo.reclassifyCalls)
		}
	})

	t.Run("discarded item cannot be reclassified", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemIA, TipoStatus: TipoStatusAConfirmar, Status: StatusDiscarded})
		uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

		_, err := uc.Reclassificar(context.Background(), "t1", "a1", "contestacao", TipoContestar)
		if !errors.Is(err, ErrActionItemDiscarded) {
			t.Fatalf("err = %v, want ErrActionItemDiscarded", err)
		}
		if repo.reclassifyCalls != 0 {
			t.Errorf("reclassify calls = %d, want 0 (guard blocked before the UPDATE)", repo.reclassifyCalls)
		}
	})

	t.Run("filed draft blocks reclassification, no UPDATE happens", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{ID: "a1", TenantID: "t1", Tipo: TipoContestar, GeraPeca: true, PieceProfileKey: "contestacao", TipoOrigem: TipoOrigemDeclarado, TipoStatus: TipoStatusConfiavel, Status: StatusConfirmed})
		repo.hasFiledDraft = true
		uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

		_, err := uc.Reclassificar(context.Background(), "t1", "a1", "apelacao", TipoRecorrer)
		if !errors.Is(err, ErrActionItemHasFiledDraft) {
			t.Fatalf("err = %v, want ErrActionItemHasFiledDraft", err)
		}
		if repo.reclassifyCalls != 0 {
			t.Errorf("reclassify calls = %d, want 0 (no UPDATE once a filed draft is found)", repo.reclassifyCalls)
		}
		unchanged := repo.items["a1"]
		if unchanged.PieceProfileKey != "contestacao" || unchanged.Tipo != TipoContestar {
			t.Errorf("item mutated despite the guard = %+v", unchanged)
		}
	})

	t.Run("same piece_profile_key+tipo already manual is an idempotent no-op", func(t *testing.T) {
		t.Parallel()
		repo := newMockRepo()
		repo.seed(&ActionItem{
			ID: "a1", TenantID: "t1", Tipo: TipoContestar, GeraPeca: true, PieceProfileKey: "contestacao",
			TipoOrigem: TipoOrigemManual, TipoStatus: TipoStatusConfiavel, Status: StatusConfirmed,
		})
		outbox := &recordingOutbox{}
		uc := NewUseCase(repo, outbox, &fakeDedup{}, &fakeUOW{})

		item, err := uc.Reclassificar(context.Background(), "t1", "a1", "contestacao", TipoContestar)
		if err != nil {
			t.Fatalf("Reclassificar() error = %v", err)
		}
		if item.TipoOrigem != TipoOrigemManual {
			t.Errorf("tipo_origem = %q, want manual (unchanged)", item.TipoOrigem)
		}
		if repo.reclassifyCalls != 0 {
			t.Errorf("reclassify calls = %d, want 0 (idempotent no-op, no re-UPDATE)", repo.reclassifyCalls)
		}
		if len(outbox.published) != 0 {
			t.Errorf("published = %d, want 0 (no re-emit on idempotent no-op)", len(outbox.published))
		}
	})

	t.Run("missing item is a typed 404", func(t *testing.T) {
		t.Parallel()
		uc := NewUseCase(newMockRepo(), &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

		_, err := uc.Reclassificar(context.Background(), "t1", "ghost", "contestacao", TipoContestar)
		if !errors.Is(err, ErrActionItemNotFound) {
			t.Fatalf("err = %v, want ErrActionItemNotFound", err)
		}
	})
}

// --- OnTaskCreated (fatia 3, the reverse pointer) ---------------------------------------

// TestOnTaskCreated_LinksTaskAndConfirms is the happy path: a task.created carrying an
// action_item_id writes task_id + status=CONFIRMED on THIS slice's own row, tenant-scoped.
func TestOnTaskCreated_LinksTaskAndConfirms(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemDeclarado, TipoStatus: TipoStatusConfiavel, Status: StatusSuggested})
	uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

	ev := TaskCreated{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", ActionItemID: "a1", TaskID: "task-1"}
	if err := uc.OnTaskCreated(context.Background(), ev); err != nil {
		t.Fatalf("OnTaskCreated() error = %v", err)
	}
	if repo.linkTaskCalls != 1 {
		t.Fatalf("LinkTask calls = %d, want 1", repo.linkTaskCalls)
	}
	linked := repo.items["a1"]
	if linked.TaskID != "task-1" {
		t.Errorf("TaskID = %q, want task-1", linked.TaskID)
	}
	if linked.Status != StatusConfirmed {
		t.Errorf("Status = %q, want CONFIRMED", linked.Status)
	}
}

// TestOnTaskCreated_SkipsWhenNoActionItemID proves a manual/avulsa task.created (the vast
// majority — no action_item_id) is skipped before any repo call or tx opens.
func TestOnTaskCreated_SkipsWhenNoActionItemID(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, uow)

	ev := TaskCreated{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", TaskID: "task-1"}
	if err := uc.OnTaskCreated(context.Background(), ev); err != nil {
		t.Fatalf("OnTaskCreated() error = %v", err)
	}
	if repo.linkTaskCalls != 0 {
		t.Errorf("LinkTask calls = %d, want 0 (no action_item_id)", repo.linkTaskCalls)
	}
	if uow.scope != "" {
		t.Errorf("uow scope = %q, want \"\" (no tx opened)", uow.scope)
	}
}

// TestOnTaskCreated_MissingOrAlreadyLinked_IsNoOp proves a LinkTask miss (a gone id, or one
// already linked — the repo's task_id IS NULL guard) is a safe no-op, not an error: a
// redelivered task.created must never fail forever.
func TestOnTaskCreated_MissingOrAlreadyLinked_IsNoOp(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	uc := NewUseCase(repo, &recordingOutbox{}, &fakeDedup{}, &fakeUOW{})

	ev := TaskCreated{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", ActionItemID: "ghost", TaskID: "task-1"}
	if err := uc.OnTaskCreated(context.Background(), ev); err != nil {
		t.Fatalf("OnTaskCreated() error = %v, want nil (safe no-op)", err)
	}
}

// TestOnTaskCreated_Dedup proves a replayed event_id never re-calls LinkTask.
func TestOnTaskCreated_Dedup(t *testing.T) {
	t.Parallel()

	repo := newMockRepo()
	repo.seed(&ActionItem{ID: "a1", TenantID: "t1", TipoOrigem: TipoOrigemDeclarado, TipoStatus: TipoStatusConfiavel, Status: StatusSuggested})
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &recordingOutbox{}, dedup, &fakeUOW{})

	ev := TaskCreated{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", ActionItemID: "a1", TaskID: "task-1"}
	if err := uc.OnTaskCreated(context.Background(), ev); err != nil {
		t.Fatalf("OnTaskCreated() error = %v", err)
	}
	if repo.linkTaskCalls != 0 {
		t.Errorf("LinkTask calls = %d, want 0 (a replay must not re-link)", repo.linkTaskCalls)
	}
}
