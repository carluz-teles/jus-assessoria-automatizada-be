package deadline

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

// --- fatia 3 contract round-trip -------------------------------------------------------
//
// actionitem's produced events (ActionItemCreated/ActionItemConfirmed) wrap an UNEXPORTED
// payload type, so this package cannot literal-construct them the way the other round-trip
// tests do (see events_test.go). Instead these fakes drive the REAL actionitem.UseCase to
// the point where it actually publishes the event — exercising the real producer
// construction code, never a hand-rolled substitute — without reopening fatia 2's frozen
// internals. Only the methods OnIntimationAnalyzed/Confirmar touch are implemented; the
// unused ones return a typed error so a future accidental use is loud, not silent.

// fakeActionItemRepo is a minimal actionitem.Repository fake, backed by an in-memory map.
type fakeActionItemRepo struct {
	items map[string]*actionitem.ActionItem
}

func newFakeActionItemRepo() *fakeActionItemRepo {
	return &fakeActionItemRepo{items: map[string]*actionitem.ActionItem{}}
}

func (f *fakeActionItemRepo) seed(a *actionitem.ActionItem) {
	cp := *a
	f.items[cp.ID] = &cp
}

func (f *fakeActionItemRepo) InsertActionItem(_ context.Context, _ database.Tx, a *actionitem.ActionItem) (*actionitem.ActionItem, error) {
	cp := *a
	f.items[cp.ID] = &cp
	out := cp
	return &out, nil
}

func (f *fakeActionItemRepo) GetActionItem(_ context.Context, _ database.Tx, tenantID, id string) (*actionitem.ActionItem, error) {
	item, ok := f.items[id]
	if !ok || item.TenantID != tenantID {
		return nil, actionitem.ErrActionItemNotFound
	}
	cp := *item
	return &cp, nil
}

func (f *fakeActionItemRepo) ConfirmActionItem(_ context.Context, _ database.Tx, tenantID, id string) (*actionitem.ActionItem, error) {
	item, ok := f.items[id]
	if !ok || item.TenantID != tenantID {
		return nil, actionitem.ErrActionItemConflict
	}
	item.TipoStatus = actionitem.TipoStatusConfiavel
	cp := *item
	return &cp, nil
}

func (f *fakeActionItemRepo) DiscardActionItem(context.Context, database.Tx, string, string) (*actionitem.ActionItem, error) {
	return nil, errors.New("fakeActionItemRepo: DiscardActionItem not exercised by this contract test")
}

func (f *fakeActionItemRepo) DeleteReplaceableActionItems(context.Context, database.Tx, string, string) error {
	return nil
}

func (f *fakeActionItemRepo) ExistsActionItemByTipo(context.Context, database.Tx, string, string, string, actionitem.TipoOrigem) (bool, error) {
	return false, nil
}

func (f *fakeActionItemRepo) LinkTask(context.Context, database.Tx, string, string, string) (*actionitem.ActionItem, error) {
	return nil, errors.New("fakeActionItemRepo: LinkTask not exercised by this contract test")
}

func (f *fakeActionItemRepo) HasFiledDraftForActionItem(context.Context, database.Tx, string, string) (bool, error) {
	return false, nil
}

func (f *fakeActionItemRepo) ReclassifyActionItem(context.Context, database.Tx, string, string, string, string) (*actionitem.ActionItem, error) {
	return nil, errors.New("fakeActionItemRepo: ReclassifyActionItem not exercised by this contract test")
}

// fakeActionItemOutbox captures every event the real use case publishes.
type fakeActionItemOutbox struct {
	published []events.Event
}

func (f *fakeActionItemOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	f.published = append(f.published, ev)
	return nil
}

// fakeActionItemDedup always reports "never seen" — this test never exercises replay.
type fakeActionItemDedup struct{}

func (fakeActionItemDedup) SeenOrMark(context.Context, database.Tx, string, string) (bool, error) {
	return false, nil
}

// fakeActionItemUOW runs fn with a nil tx (the fakes above never touch it).
type fakeActionItemUOW struct{}

func (fakeActionItemUOW) Do(_ context.Context, _ string, fn func(tx database.Tx) error) error {
	return fn(nil)
}

// TestActionItemCreated_ContractRoundTrip drives the REAL actionitem.UseCase.OnIntimationAnalyzed
// with a declarado candidate (born confiável), captures the actionitem.created it actually
// publishes, and round-trips it through THIS package's local ActionItemFact — asserting every
// field createTaskFromActionItem reads survives the wire. Guards against a silent field
// rename on the actionitem side (memória parallel-producer-consumer-roundtrip).
func TestActionItemCreated_ContractRoundTrip(t *testing.T) {
	if TypeActionItemCreated != actionitem.TypeActionItemCreated {
		t.Fatalf("consumed type %q != producer type %q", TypeActionItemCreated, actionitem.TypeActionItemCreated)
	}

	repo := newFakeActionItemRepo()
	outbox := &fakeActionItemOutbox{}
	uc := actionitem.NewUseCase(repo, outbox, fakeActionItemDedup{}, fakeActionItemUOW{})

	tenantID := uuid.NewString()
	intimationID := uuid.NewString()
	deadlineID := uuid.NewString()
	profileKey := "contestacao"

	ev := actionitem.IntimationAnalyzed{
		Base:         events.Base{EventID: uuid.NewString()},
		TenantID:     tenantID,
		IntimationID: intimationID,
		DeadlineID:   deadlineID,
		Providencias: []actionitem.ProvidenciaCandidate{
			{Tipo: actionitem.TipoContestar, GeraPeca: true, PieceProfileKey: &profileKey, Declarado: true},
		},
	}
	if err := uc.OnIntimationAnalyzed(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationAnalyzed() error = %v", err)
	}
	if len(outbox.published) != 1 {
		t.Fatalf("published = %d, want 1", len(outbox.published))
	}

	produced := outbox.published[0]
	if produced.Type() != TypeActionItemCreated {
		t.Fatalf("produced.Type() = %q, want %q", produced.Type(), TypeActionItemCreated)
	}

	raw, err := json.Marshal(produced)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}
	var got ActionItemFact
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.ActionItemID == "" {
		t.Error("ActionItemID empty, want the materialized item's id")
	}
	if got.TenantID != tenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, tenantID)
	}
	if got.IntimationID != intimationID {
		t.Errorf("IntimationID = %q, want %q", got.IntimationID, intimationID)
	}
	if got.Tipo != actionitem.TipoContestar {
		t.Errorf("Tipo = %q, want %q", got.Tipo, actionitem.TipoContestar)
	}
	if !got.GeraPeca {
		t.Error("GeraPeca = false, want true")
	}
	if got.PieceProfileKey == nil || *got.PieceProfileKey != profileKey {
		t.Errorf("PieceProfileKey = %v, want %q", got.PieceProfileKey, profileKey)
	}
	if got.DeadlineID == nil || *got.DeadlineID != deadlineID {
		t.Errorf("DeadlineID = %v, want %q", got.DeadlineID, deadlineID)
	}
}

// TestActionItemConfirmed_ContractRoundTrip mirrors the created round-trip for the confirmed
// counterpart: seeds an a_confirmar item directly, drives the REAL Confirmar, captures the
// REAL actionitem.confirmed, and round-trips it the same way.
func TestActionItemConfirmed_ContractRoundTrip(t *testing.T) {
	if TypeActionItemConfirmed != actionitem.TypeActionItemConfirmed {
		t.Fatalf("consumed type %q != producer type %q", TypeActionItemConfirmed, actionitem.TypeActionItemConfirmed)
	}

	repo := newFakeActionItemRepo()
	outbox := &fakeActionItemOutbox{}
	uc := actionitem.NewUseCase(repo, outbox, fakeActionItemDedup{}, fakeActionItemUOW{})

	tenantID := uuid.NewString()
	itemID := uuid.NewString()
	repo.seed(&actionitem.ActionItem{
		ID: itemID, TenantID: tenantID, IntimationID: uuid.NewString(), Tipo: actionitem.TipoManifestar,
		TipoOrigem: actionitem.TipoOrigemIA, TipoStatus: actionitem.TipoStatusAConfirmar, Status: actionitem.StatusSuggested,
	})

	if _, err := uc.Confirmar(context.Background(), tenantID, itemID); err != nil {
		t.Fatalf("Confirmar() error = %v", err)
	}
	if len(outbox.published) != 1 {
		t.Fatalf("published = %d, want 1", len(outbox.published))
	}

	produced := outbox.published[0]
	if produced.Type() != TypeActionItemConfirmed {
		t.Fatalf("produced.Type() = %q, want %q", produced.Type(), TypeActionItemConfirmed)
	}

	raw, err := json.Marshal(produced)
	if err != nil {
		t.Fatalf("marshal producer: %v", err)
	}
	var got ActionItemFact
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal into local shape: %v", err)
	}

	if got.ActionItemID != itemID {
		t.Errorf("ActionItemID = %q, want %q", got.ActionItemID, itemID)
	}
	if got.TenantID != tenantID {
		t.Errorf("TenantID = %q, want %q", got.TenantID, tenantID)
	}
	if got.Tipo != actionitem.TipoManifestar {
		t.Errorf("Tipo = %q, want %q", got.Tipo, actionitem.TipoManifestar)
	}
	if got.GeraPeca {
		t.Error("GeraPeca = true, want false (the seeded item)")
	}
	if got.PieceProfileKey != nil {
		t.Errorf("PieceProfileKey = %v, want nil", got.PieceProfileKey)
	}
	if got.DeadlineID != nil {
		t.Errorf("DeadlineID = %v, want nil (not bound on the seeded item)", got.DeadlineID)
	}
}

// --- createTaskFromActionItem (unit, mocked repo) ---------------------------------------

// TestOnActionItemCreated_CreatesTaskFromTipo is the happy path: the task's title is the
// providência's tipo, deadline_id/intimation_id are herdados from the payload, source is
// RULE, court_record_id comes from the decisão-P1 read, and task.created carries the
// action_item_id back for actionitem's own reverse-pointer listener.
func TestOnActionItemCreated_CreatesTaskFromTipo(t *testing.T) {
	tenantID := uuid.NewString()
	actionItemID := uuid.NewString()
	intimationID := uuid.NewString()
	deadlineID := uuid.NewString()
	courtRecordID := uuid.NewString()

	repo := &mockRepo{actionItemCourtRecordID: courtRecordID}
	outbox := &fakeOutbox{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, uow)

	ev := ActionItemFact{
		Base:         events.Base{EventID: uuid.NewString()},
		ActionItemID: actionItemID,
		TenantID:     tenantID,
		IntimationID: intimationID,
		Tipo:         "contestar",
		GeraPeca:     true,
		DeadlineID:   &deadlineID,
	}
	if err := uc.OnActionItemCreated(context.Background(), ev); err != nil {
		t.Fatalf("OnActionItemCreated() error = %v", err)
	}

	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	if repo.actionItemCourtRecordCalls != 1 || repo.gotActionItemCourtRecordID != actionItemID || repo.gotActionItemTenantID != tenantID {
		t.Errorf("GetActionItemCourtRecordID calls/id/tenant = %d/%q/%q, want 1/%q/%q",
			repo.actionItemCourtRecordCalls, repo.gotActionItemCourtRecordID, repo.gotActionItemTenantID, actionItemID, tenantID)
	}
	if repo.insertTaskCalls != 1 || len(repo.insertedTasks) != 1 {
		t.Fatalf("InsertTask calls = %d, want 1", repo.insertTaskCalls)
	}
	saved := repo.insertedTasks[0]
	if saved.Title != "contestar" {
		t.Errorf("saved.Title = %q, want %q (the tipo)", saved.Title, "contestar")
	}
	if saved.Status != TaskStatusOpen || saved.Source != SourceRule {
		t.Errorf("saved status/source = %q/%q, want OPEN/RULE", saved.Status, saved.Source)
	}
	if saved.DeadlineID != deadlineID || saved.IntimationID != intimationID || saved.CourtRecordID != courtRecordID {
		t.Errorf("saved context ids = deadline %q / intim %q / cr %q, want %q/%q/%q",
			saved.DeadlineID, saved.IntimationID, saved.CourtRecordID, deadlineID, intimationID, courtRecordID)
	}
	if saved.ActionItemID != actionItemID {
		t.Errorf("saved.ActionItemID = %q, want %q", saved.ActionItemID, actionItemID)
	}

	created := publishedOfType[TaskCreated](outbox)
	if len(created) != 1 {
		t.Fatalf("task.created events = %d, want 1", len(created))
	}
	tc := created[0]
	if tc.ActionItemID != actionItemID {
		t.Errorf("task.created.ActionItemID = %q, want %q", tc.ActionItemID, actionItemID)
	}
	if tc.TenantID != tenantID {
		t.Errorf("task.created.TenantID = %q, want %q", tc.TenantID, tenantID)
	}
}

// TestOnActionItemCreated_NoDeadlineBoundYet proves gera_peca=false / no deadline_id still
// creates a task (docs §2: "há o quê fazer: dar-se por ciente" — every providência gets a
// task, gera_peca or not), with an empty deadline_id.
func TestOnActionItemCreated_NoDeadlineBoundYet(t *testing.T) {
	repo := &mockRepo{}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	ev := ActionItemFact{
		Base:         events.Base{EventID: uuid.NewString()},
		ActionItemID: uuid.NewString(),
		TenantID:     uuid.NewString(),
		IntimationID: uuid.NewString(),
		Tipo:         "ciencia",
		GeraPeca:     false,
	}
	if err := uc.OnActionItemCreated(context.Background(), ev); err != nil {
		t.Fatalf("OnActionItemCreated() error = %v", err)
	}
	if repo.insertTaskCalls != 1 {
		t.Fatalf("InsertTask calls = %d, want 1 (ciência still gets a task)", repo.insertTaskCalls)
	}
	if repo.insertedTasks[0].DeadlineID != "" {
		t.Errorf("DeadlineID = %q, want empty (no prazo bound on this providência)", repo.insertedTasks[0].DeadlineID)
	}
	if len(publishedOfType[TaskCreated](outbox)) != 1 {
		t.Error("task.created not emitted")
	}
}

// TestOnActionItemConfirmed_SameCoreAsCreated proves actionitem.confirmed funnels into the
// exact same task-creation core as actionitem.created (docs §6: "mesmo efeito a jusante").
func TestOnActionItemConfirmed_SameCoreAsCreated(t *testing.T) {
	repo := &mockRepo{}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	ev := ActionItemFact{
		Base:         events.Base{EventID: uuid.NewString()},
		ActionItemID: uuid.NewString(),
		TenantID:     uuid.NewString(),
		IntimationID: uuid.NewString(),
		Tipo:         "manifestar",
	}
	if err := uc.OnActionItemConfirmed(context.Background(), ev); err != nil {
		t.Fatalf("OnActionItemConfirmed() error = %v", err)
	}
	if repo.insertTaskCalls != 1 || repo.insertedTasks[0].Source != SourceRule {
		t.Errorf("InsertTask calls/source = %d/%q, want 1/RULE", repo.insertTaskCalls, repo.insertedTasks[0].Source)
	}
}

// TestOnActionItemCreated_Dedup proves a replayed event_id never re-inserts nor re-emits —
// the SAME consumer-side dedup floor every other listener in this slice uses.
func TestOnActionItemCreated_Dedup(t *testing.T) {
	repo := &mockRepo{}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	ev := ActionItemFact{Base: events.Base{EventID: uuid.NewString()}, ActionItemID: uuid.NewString(), TenantID: uuid.NewString(), Tipo: "contestar"}
	if err := uc.OnActionItemCreated(context.Background(), ev); err != nil {
		t.Fatalf("OnActionItemCreated() error = %v", err)
	}
	if repo.insertTaskCalls != 0 {
		t.Errorf("InsertTask calls = %d, want 0 (a replay must not create a second task)", repo.insertTaskCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published = %d, want 0", len(outbox.published))
	}
}

// TestOnActionItemCreated_IdempotentOnTaskConflict proves the DB-level idempotency floor
// (0087's UNIQUE): if InsertTask's ON CONFLICT DO NOTHING reports ErrTaskExistsForActionItem
// (a redelivery that got past the dedup mark), the use case treats it as a safe no-op — no
// error, no re-emitted task.created.
func TestOnActionItemCreated_IdempotentOnTaskConflict(t *testing.T) {
	repo := &mockRepo{insertTaskErr: ErrTaskExistsForActionItem}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	ev := ActionItemFact{Base: events.Base{EventID: uuid.NewString()}, ActionItemID: uuid.NewString(), TenantID: uuid.NewString(), Tipo: "contestar"}
	if err := uc.OnActionItemCreated(context.Background(), ev); err != nil {
		t.Fatalf("OnActionItemCreated() error = %v, want nil (idempotent no-op)", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published = %d, want 0 (no re-emit on an existing task)", len(outbox.published))
	}
}

// TestOnActionItemCreated_CourtRecordLookupFails proves a genuine repo error (the
// decisão-P1 read) propagates — nothing is inserted or emitted on a hard fault.
func TestOnActionItemCreated_CourtRecordLookupFails(t *testing.T) {
	repo := &mockRepo{actionItemCourtRecordIDErr: ErrActionItemNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	ev := ActionItemFact{Base: events.Base{EventID: uuid.NewString()}, ActionItemID: uuid.NewString(), TenantID: uuid.NewString(), Tipo: "contestar"}
	err := uc.OnActionItemCreated(context.Background(), ev)
	if !errors.Is(err, ErrActionItemNotFound) {
		t.Fatalf("err = %v, want ErrActionItemNotFound", err)
	}
	if repo.insertTaskCalls != 0 || len(outbox.published) != 0 {
		t.Errorf("insert/published = %d/%d, want 0/0 on a failed lookup", repo.insertTaskCalls, len(outbox.published))
	}
}
