package draft

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/actionitem"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// ─── ReclassifyUseCase unit tests ───────────────────────────────────────────────

// fakeReclassifyRepo is a configurable stub satisfying reclassifyRepo.
type fakeReclassifyRepo struct {
	taskIDResult string
	taskIDErr    error
	taskIDCalls  int

	supersedeErr   error
	supersedeCalls int
	lastTaskID     string
}

func (f *fakeReclassifyRepo) GetTaskIDForActionItem(_ context.Context, _ database.Tx, _, _ string) (string, error) {
	f.taskIDCalls++
	return f.taskIDResult, f.taskIDErr
}

func (f *fakeReclassifyRepo) SupersedeDraftForTask(_ context.Context, _ database.Tx, _, taskID string) error {
	f.supersedeCalls++
	f.lastTaskID = taskID
	return f.supersedeErr
}

// fakeReclassifyDedup is a configurable SeenOrMark stub.
type fakeReclassifyDedup struct {
	seen  bool
	err   error
	calls int
}

func (d *fakeReclassifyDedup) SeenOrMark(_ context.Context, _ database.Tx, _, _ string) (bool, error) {
	d.calls++
	return d.seen, d.err
}

func TestReclassifyUseCase_OnActionItemReclassified(t *testing.T) {
	t.Parallel()

	t.Run("resolves task and supersedes its vigente draft", func(t *testing.T) {
		t.Parallel()
		repo := &fakeReclassifyRepo{taskIDResult: "task-1"}
		uow := &fakeUOW{}
		uc := NewReclassifyUseCase(uow, repo, &fakeReclassifyDedup{})

		ev := ActionItemReclassified{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", ActionItemID: "a1"}
		if err := uc.OnActionItemReclassified(context.Background(), ev); err != nil {
			t.Fatalf("OnActionItemReclassified() error = %v", err)
		}
		if repo.taskIDCalls != 1 {
			t.Errorf("GetTaskIDForActionItem calls = %d, want 1", repo.taskIDCalls)
		}
		if repo.supersedeCalls != 1 {
			t.Fatalf("SupersedeDraftForTask calls = %d, want 1", repo.supersedeCalls)
		}
		if repo.lastTaskID != "task-1" {
			t.Errorf("SupersedeDraftForTask taskID = %q, want task-1", repo.lastTaskID)
		}
		if len(uow.scopes) == 0 || uow.scopes[0] != "t1" {
			t.Errorf("uow scope = %v, want [t1]", uow.scopes)
		}
	})

	t.Run("no task yet is a safe no-op — never calls Supersede", func(t *testing.T) {
		t.Parallel()
		repo := &fakeReclassifyRepo{taskIDResult: ""}
		uc := NewReclassifyUseCase(&fakeUOW{}, repo, &fakeReclassifyDedup{})

		ev := ActionItemReclassified{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", ActionItemID: "a1"}
		if err := uc.OnActionItemReclassified(context.Background(), ev); err != nil {
			t.Fatalf("OnActionItemReclassified() error = %v", err)
		}
		if repo.supersedeCalls != 0 {
			t.Errorf("SupersedeDraftForTask calls = %d, want 0 (providência has no task yet)", repo.supersedeCalls)
		}
	})

	t.Run("dedup skips a replayed event — never re-resolves or re-supersedes", func(t *testing.T) {
		t.Parallel()
		repo := &fakeReclassifyRepo{taskIDResult: "task-1"}
		dedup := &fakeReclassifyDedup{seen: true}
		uc := NewReclassifyUseCase(&fakeUOW{}, repo, dedup)

		ev := ActionItemReclassified{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", ActionItemID: "a1"}
		if err := uc.OnActionItemReclassified(context.Background(), ev); err != nil {
			t.Fatalf("OnActionItemReclassified() error = %v", err)
		}
		if repo.taskIDCalls != 0 {
			t.Errorf("GetTaskIDForActionItem calls = %d, want 0 (a replay must not re-resolve)", repo.taskIDCalls)
		}
		if repo.supersedeCalls != 0 {
			t.Errorf("SupersedeDraftForTask calls = %d, want 0 (a replay must not re-supersede)", repo.supersedeCalls)
		}
	})

	t.Run("GetTaskIDForActionItem infra error propagates", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("infra boom")
		repo := &fakeReclassifyRepo{taskIDErr: wantErr}
		uc := NewReclassifyUseCase(&fakeUOW{}, repo, &fakeReclassifyDedup{})

		ev := ActionItemReclassified{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", ActionItemID: "a1"}
		if err := uc.OnActionItemReclassified(context.Background(), ev); !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	})

	t.Run("SupersedeDraftForTask infra error propagates", func(t *testing.T) {
		t.Parallel()
		wantErr := errors.New("infra boom")
		repo := &fakeReclassifyRepo{taskIDResult: "task-1", supersedeErr: wantErr}
		uc := NewReclassifyUseCase(&fakeUOW{}, repo, &fakeReclassifyDedup{})

		ev := ActionItemReclassified{Base: events.Base{EventID: uuid.NewString()}, TenantID: "t1", ActionItemID: "a1"}
		if err := uc.OnActionItemReclassified(context.Background(), ev); !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
	})
}

// ─── Contract round-trip (memória parallel-producer-consumer-roundtrip) ────────────
//
// actionitem's produced events (ActionItemReclassified included) wrap an UNEXPORTED
// payload type, so this package cannot literal-construct one directly (mirrors
// internal/deadline/actionitem_task_test.go's rationale for the SAME family of events).
// Instead this drives the REAL actionitem.UseCase.Reclassificar to the point where it
// actually publishes the event, then round-trips the REAL produced value through this
// slice's LOCAL ActionItemReclassified decode shape.

// fakeActionItemRepoForDraft is a minimal actionitem.Repository fake, backed by an
// in-memory map — enough to drive Reclassificar without a real database. Every method
// GetActionItem/ReclassifyActionItem/HasFiledDraftForActionItem needs is implemented; the
// rest return a typed error so an accidental future use is loud, not silent.
type fakeActionItemRepoForDraft struct {
	items map[string]*actionitem.ActionItem
}

func newFakeActionItemRepoForDraft() *fakeActionItemRepoForDraft {
	return &fakeActionItemRepoForDraft{items: map[string]*actionitem.ActionItem{}}
}

func (f *fakeActionItemRepoForDraft) seed(a *actionitem.ActionItem) {
	cp := *a
	f.items[cp.ID] = &cp
}

func (f *fakeActionItemRepoForDraft) InsertActionItem(context.Context, database.Tx, *actionitem.ActionItem) (*actionitem.ActionItem, error) {
	return nil, errors.New("fakeActionItemRepoForDraft: InsertActionItem not exercised by this contract test")
}

func (f *fakeActionItemRepoForDraft) GetActionItem(_ context.Context, _ database.Tx, tenantID, id string) (*actionitem.ActionItem, error) {
	item, ok := f.items[id]
	if !ok || item.TenantID != tenantID {
		return nil, actionitem.ErrActionItemNotFound
	}
	cp := *item
	return &cp, nil
}

func (f *fakeActionItemRepoForDraft) ConfirmActionItem(context.Context, database.Tx, string, string) (*actionitem.ActionItem, error) {
	return nil, errors.New("fakeActionItemRepoForDraft: ConfirmActionItem not exercised by this contract test")
}

func (f *fakeActionItemRepoForDraft) DiscardActionItem(context.Context, database.Tx, string, string) (*actionitem.ActionItem, error) {
	return nil, errors.New("fakeActionItemRepoForDraft: DiscardActionItem not exercised by this contract test")
}

func (f *fakeActionItemRepoForDraft) DeleteReplaceableActionItems(context.Context, database.Tx, string, string) error {
	return nil
}

func (f *fakeActionItemRepoForDraft) ExistsActionItemByTipo(context.Context, database.Tx, string, string, string) (bool, error) {
	return false, nil
}

func (f *fakeActionItemRepoForDraft) LinkTask(context.Context, database.Tx, string, string, string) (*actionitem.ActionItem, error) {
	return nil, errors.New("fakeActionItemRepoForDraft: LinkTask not exercised by this contract test")
}

func (f *fakeActionItemRepoForDraft) HasFiledDraftForActionItem(context.Context, database.Tx, string, string) (bool, error) {
	return false, nil
}

func (f *fakeActionItemRepoForDraft) ReclassifyActionItem(_ context.Context, _ database.Tx, tenantID, id, pieceProfileKey, tipo string) (*actionitem.ActionItem, error) {
	item, ok := f.items[id]
	if !ok || item.TenantID != tenantID {
		return nil, actionitem.ErrActionItemConflict
	}
	item.PieceProfileKey = pieceProfileKey
	item.Tipo = tipo
	item.TipoOrigem = actionitem.TipoOrigemManual
	item.TipoStatus = actionitem.TipoStatusConfiavel
	item.GeraPeca = true
	item.Confianca = nil
	cp := *item
	return &cp, nil
}

// fakeActionItemOutboxForDraft captures every event the real use case publishes.
type fakeActionItemOutboxForDraft struct {
	published []events.Event
}

func (f *fakeActionItemOutboxForDraft) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	f.published = append(f.published, ev)
	return nil
}

// fakeActionItemDedupForDraft always reports "never seen" — this test never exercises replay.
type fakeActionItemDedupForDraft struct{}

func (fakeActionItemDedupForDraft) SeenOrMark(context.Context, database.Tx, string, string) (bool, error) {
	return false, nil
}

// fakeActionItemUOWForDraft runs fn with a nil tx (the fakes above never touch it).
type fakeActionItemUOWForDraft struct{}

func (fakeActionItemUOWForDraft) Do(_ context.Context, _ string, fn func(tx database.Tx) error) error {
	return fn(nil)
}

// TestActionItemReclassified_ContractRoundTrip drives the REAL
// actionitem.UseCase.Reclassificar, captures the actionitem.reclassified it actually
// publishes, and round-trips it through THIS package's local ActionItemReclassified —
// asserting every field the reclassify listener reads survives the wire. Guards against a
// silent field rename on the actionitem side.
func TestActionItemReclassified_ContractRoundTrip(t *testing.T) {
	if TypeActionItemReclassified != actionitem.TypeActionItemReclassified {
		t.Fatalf("consumed type %q != producer type %q", TypeActionItemReclassified, actionitem.TypeActionItemReclassified)
	}

	repo := newFakeActionItemRepoForDraft()
	outbox := &fakeActionItemOutboxForDraft{}
	uc := actionitem.NewUseCase(repo, outbox, fakeActionItemDedupForDraft{}, fakeActionItemUOWForDraft{})

	tenantID := uuid.NewString()
	itemID := uuid.NewString()
	repo.seed(&actionitem.ActionItem{
		ID: itemID, TenantID: tenantID, IntimationID: uuid.NewString(), Tipo: actionitem.TipoManifestar,
		TipoOrigem: actionitem.TipoOrigemIA, TipoStatus: actionitem.TipoStatusAConfirmar, Status: actionitem.StatusSuggested,
	})

	if _, err := uc.Reclassificar(context.Background(), tenantID, itemID, "contestacao", actionitem.TipoContestar); err != nil {
		t.Fatalf("Reclassificar() error = %v", err)
	}
	if len(outbox.published) != 1 {
		t.Fatalf("published = %d, want 1", len(outbox.published))
	}

	produced := outbox.published[0]
	if produced.Type() != TypeActionItemReclassified {
		t.Fatalf("produced.Type() = %q, want %q", produced.Type(), TypeActionItemReclassified)
	}

	raw, err := json.Marshal(produced)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}
	var got ActionItemReclassified
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.ActionItemID != itemID {
		t.Errorf("ActionItemID = %q, want %q", got.ActionItemID, itemID)
	}
	if got.TenantID != tenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, tenantID)
	}
	if got.EventID == "" {
		t.Error("EventID empty, want a fresh event id (dedup key)")
	}
}
