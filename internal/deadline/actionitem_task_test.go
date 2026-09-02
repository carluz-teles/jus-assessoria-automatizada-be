package deadline

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/actionitem"
	"github.com/jusassessoria/platform/lib/database"
)

// actionitem_task_test.go covers the SYNCHRONOUS providência→tarefa adapter
// (ActionItemTaskCreator): internal/actionitem calls it inside its own tx to mint (or find,
// idempotently) the tarefa of a confiável providência. The old async listener flow
// (actionitem.created/confirmed → InsertTask → task.created → link) is gone; the task write now
// commits in the caller's tx with no event hop.

// fakeTaskCreatorRepo is a minimal taskCreatorRepo fake: it records the inserted task, can be
// told to report ErrTaskExistsForActionItem, and serves a fixed existing task id for the
// idempotent fallback read.
type fakeTaskCreatorRepo struct {
	insertCalls   int
	inserted      *Task
	insertErr     error
	existingID    string
	getByItemErr  error
	getByItemCall int
}

func (f *fakeTaskCreatorRepo) InsertTask(_ context.Context, _ database.Tx, t *Task) (*Task, error) {
	f.insertCalls++
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	cp := *t
	cp.ID = uuid.NewString()
	f.inserted = &cp
	out := cp
	return &out, nil
}

func (f *fakeTaskCreatorRepo) GetTaskIDByActionItem(_ context.Context, _ database.Tx, _, _ string) (string, error) {
	f.getByItemCall++
	if f.getByItemErr != nil {
		return "", f.getByItemErr
	}
	return f.existingID, nil
}

// TestActionItemTaskCreator_CreatesTaskFromInput is the happy path: the task is born OPEN/RULE,
// titled after the providência's tipo, inheriting the context ids the caller passes directly
// (no cross-table read), and its DB-assigned id is returned for the caller to link.
func TestActionItemTaskCreator_CreatesTaskFromInput(t *testing.T) {
	repo := &fakeTaskCreatorRepo{}
	c := NewActionItemTaskCreator(repo)

	in := actionitem.ActionItemTask{
		TenantID:      uuid.NewString(),
		ActionItemID:  uuid.NewString(),
		CourtRecordID: uuid.NewString(),
		DeadlineID:    uuid.NewString(),
		IntimationID:  uuid.NewString(),
		Tipo:          "contestar",
	}
	taskID, err := c.CreateForActionItem(context.Background(), nil, in)
	if err != nil {
		t.Fatalf("CreateForActionItem() error = %v", err)
	}
	if repo.insertCalls != 1 {
		t.Fatalf("InsertTask calls = %d, want 1", repo.insertCalls)
	}
	saved := repo.inserted
	if saved.Title != "contestar" {
		t.Errorf("Title = %q, want %q (the tipo)", saved.Title, "contestar")
	}
	if saved.Status != TaskStatusOpen || saved.Source != SourceRule {
		t.Errorf("status/source = %q/%q, want OPEN/RULE", saved.Status, saved.Source)
	}
	if saved.CourtRecordID != in.CourtRecordID || saved.DeadlineID != in.DeadlineID ||
		saved.IntimationID != in.IntimationID || saved.ActionItemID != in.ActionItemID {
		t.Errorf("context ids = %+v, want the input's", saved)
	}
	if taskID != saved.ID {
		t.Errorf("returned taskID = %q, want the DB-assigned %q", taskID, saved.ID)
	}
}

// TestActionItemTaskCreator_NoContextIDs proves a providência with no prazo/intimação/court bound
// still creates a task (docs §2: "há o quê fazer: dar-se por ciente") with empty context ids.
func TestActionItemTaskCreator_NoContextIDs(t *testing.T) {
	repo := &fakeTaskCreatorRepo{}
	c := NewActionItemTaskCreator(repo)

	_, err := c.CreateForActionItem(context.Background(), nil, actionitem.ActionItemTask{
		TenantID:     uuid.NewString(),
		ActionItemID: uuid.NewString(),
		Tipo:         "ciencia",
	})
	if err != nil {
		t.Fatalf("CreateForActionItem() error = %v", err)
	}
	if repo.insertCalls != 1 {
		t.Fatalf("InsertTask calls = %d, want 1 (ciência still gets a task)", repo.insertCalls)
	}
	if repo.inserted.DeadlineID != "" {
		t.Errorf("DeadlineID = %q, want empty", repo.inserted.DeadlineID)
	}
}

// TestActionItemTaskCreator_IdempotentOnConflict proves the DB-level idempotency floor (0087's
// UNIQUE): when InsertTask reports ErrTaskExistsForActionItem, the adapter reads back and returns
// the EXISTING task id instead of minting a second — so the caller links the same task.
func TestActionItemTaskCreator_IdempotentOnConflict(t *testing.T) {
	existing := uuid.NewString()
	repo := &fakeTaskCreatorRepo{insertErr: ErrTaskExistsForActionItem, existingID: existing}
	c := NewActionItemTaskCreator(repo)

	taskID, err := c.CreateForActionItem(context.Background(), nil, actionitem.ActionItemTask{
		TenantID:     uuid.NewString(),
		ActionItemID: uuid.NewString(),
		Tipo:         "contestar",
	})
	if err != nil {
		t.Fatalf("CreateForActionItem() error = %v, want nil (idempotent)", err)
	}
	if repo.getByItemCall != 1 {
		t.Fatalf("GetTaskIDByActionItem calls = %d, want 1 (fallback read on conflict)", repo.getByItemCall)
	}
	if taskID != existing {
		t.Errorf("taskID = %q, want the existing %q", taskID, existing)
	}
}

// TestActionItemTaskCreator_InsertErrorPropagates proves a genuine insert fault (not the
// conflict) propagates unchanged — nothing is swallowed.
func TestActionItemTaskCreator_InsertErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	repo := &fakeTaskCreatorRepo{insertErr: boom}
	c := NewActionItemTaskCreator(repo)

	_, err := c.CreateForActionItem(context.Background(), nil, actionitem.ActionItemTask{
		TenantID:     uuid.NewString(),
		ActionItemID: uuid.NewString(),
		Tipo:         "contestar",
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the insert error", err)
	}
	if repo.getByItemCall != 0 {
		t.Errorf("GetTaskIDByActionItem calls = %d, want 0 (only on conflict)", repo.getByItemCall)
	}
}
