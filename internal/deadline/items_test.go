package deadline

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/apperr"
)

// --- CreateTaskItem ---------------------------------------------------------

// TestCreateTaskItem_GuardsParentThenAppends is the happy path for POST /v1/tasks/:id/items: the
// parent-task guard runs FIRST (a foreign :id would 404 before any insert), the append position is
// forwarded, and the item is inserted born done=false in a tenant-scoped tx.
func TestCreateTaskItem_GuardsParentThenAppends(t *testing.T) {
	tenantID := uuid.NewString()
	taskID := uuid.NewString()
	itemID := uuid.NewString()

	repo := &mockRepo{
		nextItemPosition: 3,
		insertedItem:     &TaskItem{ID: itemID, TaskID: taskID, Title: "Redigir", Position: 3},
	}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, uow)

	item, err := uc.CreateTaskItem(context.Background(), CreateTaskItemCommand{
		TenantID: tenantID, TaskID: taskID, Title: "Redigir",
	})
	if err != nil {
		t.Fatalf("CreateTaskItem() error = %v", err)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	// Guard first, keyed by (task, tenant).
	if repo.ensureTaskCalls != 1 || repo.gotEnsureTaskID != taskID || repo.gotEnsureTenantID != tenantID {
		t.Errorf("EnsureTaskInTenant calls/task/tenant = %d/%q/%q, want 1/%q/%q",
			repo.ensureTaskCalls, repo.gotEnsureTaskID, repo.gotEnsureTenantID, taskID, tenantID)
	}
	if repo.nextPosCalls != 1 || repo.gotNextPosTaskID != taskID {
		t.Errorf("NextTaskItemPosition calls/task = %d/%q, want 1/%q", repo.nextPosCalls, repo.gotNextPosTaskID, taskID)
	}
	if repo.insertItemCalls != 1 {
		t.Fatalf("InsertTaskItem calls = %d, want 1", repo.insertItemCalls)
	}
	inserted := repo.insertedItems[0]
	if inserted.TenantID != tenantID || inserted.TaskID != taskID || inserted.Title != "Redigir" {
		t.Errorf("inserted item = %+v, want tenant/task/title carried", inserted)
	}
	if inserted.Position != 3 || inserted.Done {
		t.Errorf("inserted position/done = %d/%v, want 3/false (append, born undone)", inserted.Position, inserted.Done)
	}
	if item.ID != itemID {
		t.Errorf("returned item id = %q, want %q", item.ID, itemID)
	}
}

// TestCreateTaskItem_ParentMissing404 proves a foreign/unknown parent task is the guard's typed
// ErrTaskItemNotFound (→ 404): no position lookup, no insert.
func TestCreateTaskItem_ParentMissing404(t *testing.T) {
	repo := &mockRepo{ensureTaskErr: ErrTaskItemNotFound}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	_, err := uc.CreateTaskItem(context.Background(), CreateTaskItemCommand{
		TenantID: uuid.NewString(), TaskID: uuid.NewString(), Title: "x",
	})
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if repo.nextPosCalls != 0 || repo.insertItemCalls != 0 {
		t.Errorf("nextPos/insert ran past a missing parent: %d/%d", repo.nextPosCalls, repo.insertItemCalls)
	}
}

// --- UpdateTaskItem ---------------------------------------------------------

// TestUpdateTaskItem_TogglingDoneStampsDoneAt proves ticking an item done (present done=true)
// stamps done_at=now, keeps the stored title when absent, and merges in a tenant-scoped tx.
func TestUpdateTaskItem_TogglingDoneStampsDoneAt(t *testing.T) {
	tenantID := uuid.NewString()
	taskID := uuid.NewString()
	itemID := uuid.NewString()
	now := time.Date(2024, 3, 20, 9, 30, 0, 0, time.UTC)

	repo := &mockRepo{
		itemForUpdate: &TaskItemForUpdate{ID: itemID, TaskID: taskID, Title: "Redigir", Done: false},
		updatedItem:   &TaskItem{ID: itemID, TaskID: taskID, Title: "Redigir", Done: true, DoneAt: &now},
	}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{}, WithClock(func() time.Time { return now }))

	done := true
	saved, err := uc.UpdateTaskItem(context.Background(), UpdateTaskItemCommand{
		TenantID: tenantID, TaskID: taskID, ItemID: itemID, Done: &done, // title absent
	})
	if err != nil {
		t.Fatalf("UpdateTaskItem() error = %v", err)
	}
	if repo.gotItemForUpdateID != itemID || repo.gotItemForUpdateTask != taskID {
		t.Errorf("GetTaskItemForUpdate item/task = %q/%q, want %q/%q", repo.gotItemForUpdateID, repo.gotItemForUpdateTask, itemID, taskID)
	}
	p := repo.gotUpdateItemParams
	if p.ItemID != itemID || p.TaskID != taskID || p.TenantID != tenantID {
		t.Errorf("update keyed by item/task/tenant = %q/%q/%q", p.ItemID, p.TaskID, p.TenantID)
	}
	if p.Title != "Redigir" {
		t.Errorf("title = %q, want Redigir (kept from storage, absent in patch)", p.Title)
	}
	if !p.Done || p.DoneAt == nil || !p.DoneAt.Equal(now) {
		t.Errorf("done/done_at = %v/%v, want true/%v (stamped now)", p.Done, p.DoneAt, now)
	}
	if saved.DoneAt == nil {
		t.Error("returned item lost its done_at")
	}
}

// TestUpdateTaskItem_UntickingClearsDoneAt proves un-ticking an item (present done=false) clears
// done_at (re-opening a step drops its completion time).
func TestUpdateTaskItem_UntickingClearsDoneAt(t *testing.T) {
	itemID := uuid.NewString()
	taskID := uuid.NewString()
	was := time.Date(2024, 3, 19, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		itemForUpdate: &TaskItemForUpdate{ID: itemID, TaskID: taskID, Title: "Redigir", Done: true},
		updatedItem:   &TaskItem{ID: itemID, TaskID: taskID, Title: "Redigir"},
	}
	_ = was
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	done := false
	if _, err := uc.UpdateTaskItem(context.Background(), UpdateTaskItemCommand{
		TenantID: uuid.NewString(), TaskID: taskID, ItemID: itemID, Done: &done,
	}); err != nil {
		t.Fatalf("UpdateTaskItem() error = %v", err)
	}
	p := repo.gotUpdateItemParams
	if p.Done {
		t.Errorf("done = %v, want false (unticked)", p.Done)
	}
	if p.DoneAt != nil {
		t.Errorf("done_at = %v, want nil (cleared on untick)", p.DoneAt)
	}
}

// TestUpdateTaskItem_RenamesKeepingDone proves renaming only (title present, done absent) keeps the
// stored done state AND its done_at (an already-done item keeps its completion time on a rename).
func TestUpdateTaskItem_RenamesKeepingDone(t *testing.T) {
	itemID := uuid.NewString()
	taskID := uuid.NewString()
	now := time.Date(2024, 3, 20, 9, 30, 0, 0, time.UTC)

	repo := &mockRepo{
		itemForUpdate: &TaskItemForUpdate{ID: itemID, TaskID: taskID, Title: "old", Done: true},
		updatedItem:   &TaskItem{ID: itemID, TaskID: taskID, Title: "new", Done: true},
	}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{}, WithClock(func() time.Time { return now }))

	title := "new"
	if _, err := uc.UpdateTaskItem(context.Background(), UpdateTaskItemCommand{
		TenantID: uuid.NewString(), TaskID: taskID, ItemID: itemID, Title: &title, // done absent → kept true
	}); err != nil {
		t.Fatalf("UpdateTaskItem() error = %v", err)
	}
	p := repo.gotUpdateItemParams
	if p.Title != "new" || !p.Done {
		t.Errorf("title/done = %q/%v, want new/true (done kept)", p.Title, p.Done)
	}
	// done stayed true, so done_at is (re)stamped from the clock — a kept-done item stays done_at.
	if p.DoneAt == nil || !p.DoneAt.Equal(now) {
		t.Errorf("done_at = %v, want %v (kept-done stays stamped)", p.DoneAt, now)
	}
}

// TestUpdateTaskItem_NotFound proves patching an unknown/cross-task item is the repo's typed
// ErrTaskItemNotFound (→ 404): nothing is updated.
func TestUpdateTaskItem_NotFound(t *testing.T) {
	repo := &mockRepo{itemForUpdateErr: ErrTaskItemNotFound}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	title := "x"
	_, err := uc.UpdateTaskItem(context.Background(), UpdateTaskItemCommand{
		TenantID: uuid.NewString(), TaskID: uuid.NewString(), ItemID: uuid.NewString(), Title: &title,
	})
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if repo.updateItemCalls != 0 {
		t.Errorf("UpdateTaskItem ran on a miss: %d", repo.updateItemCalls)
	}
}

// --- DeleteTaskItem ---------------------------------------------------------

// TestDeleteTaskItem_ForwardsKeyInTx proves delete runs in a tenant-scoped tx keyed by (item, task,
// tenant) and passes through the repo's success.
func TestDeleteTaskItem_ForwardsKeyInTx(t *testing.T) {
	tenantID := uuid.NewString()
	taskID := uuid.NewString()
	itemID := uuid.NewString()
	repo := &mockRepo{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, uow)

	if err := uc.DeleteTaskItem(context.Background(), tenantID, taskID, itemID); err != nil {
		t.Fatalf("DeleteTaskItem() error = %v", err)
	}
	if len(uow.scopes) != 1 || uow.scopes[0] != tenantID {
		t.Errorf("uow scopes = %v, want [%q]", uow.scopes, tenantID)
	}
	if repo.deleteItemCalls != 1 || repo.gotDeleteItemID != itemID || repo.gotDeleteItemTask != taskID || repo.gotDeleteItemTenant != tenantID {
		t.Errorf("delete calls/item/task/tenant = %d/%q/%q/%q, want 1/%q/%q/%q",
			repo.deleteItemCalls, repo.gotDeleteItemID, repo.gotDeleteItemTask, repo.gotDeleteItemTenant, itemID, taskID, tenantID)
	}
}

// TestDeleteTaskItem_NotFound proves deleting an unknown/foreign item is the repo's typed
// ErrTaskItemNotFound (→ 404).
func TestDeleteTaskItem_NotFound(t *testing.T) {
	repo := &mockRepo{deleteItemErr: ErrTaskItemNotFound}
	uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	err := uc.DeleteTaskItem(context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString())
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
}
