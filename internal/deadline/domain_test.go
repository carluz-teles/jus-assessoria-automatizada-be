package deadline

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/advisory"
	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/calendar"
	"github.com/jusassessoria/platform/lib/database"
	"github.com/jusassessoria/platform/lib/events"
)

// --- fakes ------------------------------------------------------------------

// mockRepo is a hand-rolled Repository: each method returns a configured value and
// records what it was asked, so a test can drive the rule/rito and assert the derived
// prazo. InsertDeadline echoes the entity back with a real uuid id (as the DB would).
type mockRepo struct {
	class    string
	classErr error

	rule    DeadlineRule
	ruleErr error

	insertID  string
	insertErr error

	revokeResult *RevokedDeadline
	revokeErr    error

	checkResult *DeadlineForCheck
	checkErr    error

	markMissedID  string
	markMissedErr error

	// docket-entry reconcile path
	reconcilable       []ReconcilableDeadline
	reconcilableErr    error
	hasResponse        bool
	hasResponseErr     error
	hasResponseByStart map[string]bool  // keyed by start_date (time.DateOnly) → per-prazo override
	markMetID          string           // returned id when no per-id override; defaults to the input id
	markMetErr         error            // blanket MarkMet error
	markMetErrByID     map[string]error // per-deadline-id MarkMet error (e.g. racing flip → ErrDeadlineNotFound)
	gotReconcileRecord string
	gotReconcileTenant string
	reconcilableCalls  int
	gotHasRespRecord   string
	gotHasRespTenant   string
	gotHasRespCodes    []int32
	hasResponseCalls   int
	markMetIDs         []string
	markMetTenantIDs   []string

	// confirm path
	confirmAnchor    *DeadlineForConfirm
	confirmAnchorErr error
	courtRecordCourt string
	courtRecordErr   error
	confirmID        string
	confirmRecordID  string
	confirmErr       error
	insertTaskErr    error

	// feedback loop (suggestion delta at confirm, camada 2)
	latestSuggestion      SuggestionRecord
	latestSuggestionOK    bool
	latestSuggestionErr   error
	latestSuggestionCalls int
	taskTitles            []string
	taskTitlesErr         error
	taskTitlesCalls       int
	gotTitlesDeadlineID   string
	gotTitlesTenantID     string

	// adjust + manual transition path (5c)
	adjustResult       *DeadlineForAdjust
	adjustErr          error
	updateAdjustID     string
	updateAdjustRecord string
	updateAdjustErr    error
	markStatusID       string
	markStatusErr      error

	// confirmation panel (0049): anchors, preview, no-deadline / reopen transitions
	anchors             IntimationAnchors
	anchorsErr          error
	anchorsCalls        int
	gotAnchorsIntim     string
	gotAnchorsTenant    string
	previewContext      PreviewContext
	previewContextErr   error
	previewContextCalls int
	gotPreviewIntim     string
	gotPreviewTenant    string
	noDeadlineID        string
	noDeadlineErr       error
	noDeadlineCalls     int
	gotNoDeadlineID     string
	gotNoDeadlineTenant string
	gotNoDeadlineBy     string
	gotNoDeadlineAt     time.Time
	reopenID            string
	reopenErr           error
	reopenCalls         int
	gotReopenID         string
	gotReopenTenant     string

	// task write path (5b)
	taskForUpdate      *TaskForUpdate
	taskForUpdateErr   error
	deadlineEndDate    time.Time
	deadlineEndDateErr error

	// action_item → task automatic creation path (fatia 3)
	actionItemCourtRecordID    string
	actionItemCourtRecordIDErr error
	gotActionItemCourtRecordID string
	gotActionItemTenantID      string
	actionItemCourtRecordCalls int
	taskIDByActionItem         string
	taskIDByActionItemErr      error

	// herança intimação → tarefa (POST /v1/tasks snapshot). intimationAssignee backs
	// GetIntimationAssignee's answer (nil = unassigned intimação); intimationAssigneeErr
	// forces the not-found branch.
	intimationAssignee    *string
	intimationAssigneeErr error
	gotIntimationAssignID string
	intimationAssignCalls int
	updatedTask           *Task
	updateTaskErr         error
	taskTransition        TaskStatus
	taskTransitionErr     error
	markTaskStatusID      string
	markTaskStatusErr     error

	// task_item write path (checklist / subtarefas, 0031)
	ensureTaskErr    error
	nextItemPosition int
	nextItemPosErr   error
	insertedItem     *TaskItem
	insertItemErr    error
	itemForUpdate    *TaskItemForUpdate
	itemForUpdateErr error
	updatedItem      *TaskItem
	updateItemErr    error
	deleteItemErr    error

	// task_comment + task_activity write path (0054/0055)
	ensureTaskExistsErr error
	insertedComment     *TaskComment
	insertCommentErr    error
	insertedActivities  []*TaskActivity
	insertActivityErr   error

	// V1 audit trail capture
	gotCalcMemory      *CalcMemory
	gotCrossValidation *CrossValidation
	gotDeadlineEvent   *DeadlineEvent
	gotAppliedHolidays []*AppliedHoliday
	policy             DeadlinePolicy

	// V1 apuração (apurar.go): ApurarDivergencia repo doubles
	crossValidation              *CrossValidation
	crossValidationErr           error
	gotGetCrossValidationTenant  string
	gotGetCrossValidationID      string
	updateCVDecisionErr          error
	updateCVDecisionCalls        int
	gotUpdateCVDecisionTenant    string
	gotUpdateCVDecisionID        string
	gotUpdateCVDecisionDecisao   string
	gotUpdateCVDecisionBy        string
	updateEndDateErr             error
	updateEndDateCalls           int
	gotUpdateEndDate             time.Time
	gotUpdateEndDatePrazoInterno time.Time
	updateSeloErr                error
	updateSeloCalls              int
	gotUpdateSelo                Seal
	gotUpdateSeloConfirmedBy     string
	gotUpdateSeloConfirmedAt     time.Time

	// captured inputs
	gotClassTenantID         string
	gotClassRecordID         string
	gotRuleType              string
	gotRuleCourt             string
	gotRuleVersion           string
	inserted                 *Deadline
	insertCalls              int
	gotRevokeIntimationID    string
	gotRevokeTenantID        string
	revokeCalls              int
	gotCheckID               string
	gotCheckTenantID         string
	checkCalls               int
	gotMissedID              string
	gotMissedTenantID        string
	markMissedCalls          int
	gotConfirmIntimation     string
	gotConfirmTenantID       string
	confirmAnchorCalls       int
	gotCourtRecordID         string
	gotCourtTenantID         string
	courtCalls               int
	gotConfirmParams         ConfirmDeadlineParams
	confirmCalls             int
	insertedTasks            []*Task
	insertTaskCalls          int
	gotAdjustID              string
	gotAdjustTenantID        string
	adjustReadCalls          int
	gotUpdateAdjustParams    UpdateDeadlineAdjustParams
	updateAdjustCalls        int
	gotMarkStatusID          string
	gotMarkStatusTenantID    string
	gotMarkStatusFrom        Status
	gotMarkStatusTo          Status
	markStatusCalls          int
	gotTaskUpdateID          string
	gotTaskUpdateTenantID    string
	taskForUpdateCalls       int
	gotDeadlineEndDateID     string
	gotDeadlineEndDateTenant string
	deadlineEndDateCalls     int
	gotUpdateTaskParams      UpdateTaskParams
	updateTaskCalls          int
	gotTaskTransitionID      string
	gotTaskTransitionTID     string
	taskTransitionCalls      int
	gotMarkTaskID            string
	gotMarkTaskTenantID      string
	gotMarkTaskFrom          TaskStatus
	gotMarkTaskTo            TaskStatus
	gotMarkTaskCompleted     *time.Time
	markTaskStatusCalls      int
	gotEnsureTaskID          string
	gotEnsureTenantID        string
	ensureTaskCalls          int
	gotNextPosTaskID         string
	nextPosCalls             int
	insertedItems            []*TaskItem
	insertItemCalls          int
	gotItemForUpdateID       string
	gotItemForUpdateTask     string
	itemForUpdateCalls       int
	gotUpdateItemParams      UpdateTaskItemParams
	updateItemCalls          int
	gotDeleteItemID          string
	gotDeleteItemTask        string
	gotDeleteItemTenant      string
	deleteItemCalls          int
}

func (m *mockRepo) GetCourtRecordClass(_ context.Context, _ database.Tx, tenantID, courtRecordID string) (string, error) {
	m.gotClassTenantID = tenantID
	m.gotClassRecordID = courtRecordID
	return m.class, m.classErr
}

func (m *mockRepo) ResolveRule(_ context.Context, _ database.Tx, rulesVersion, intimationType, court string) (DeadlineRule, error) {
	m.gotRuleVersion, m.gotRuleType, m.gotRuleCourt = rulesVersion, intimationType, court
	return m.rule, m.ruleErr
}

func (m *mockRepo) InsertDeadline(_ context.Context, _ database.Tx, d *Deadline) (*Deadline, error) {
	m.insertCalls++
	m.inserted = d
	if m.insertErr != nil {
		return nil, m.insertErr
	}
	saved := *d
	saved.ID = m.insertID
	return &saved, nil
}

func (m *mockRepo) RevokeDeadlineByIntimation(_ context.Context, _ database.Tx, intimationID, tenantID string) (*RevokedDeadline, error) {
	m.revokeCalls++
	m.gotRevokeIntimationID = intimationID
	m.gotRevokeTenantID = tenantID
	return m.revokeResult, m.revokeErr
}

// GetActionItemCourtRecordID returns the configured court_record_id, recording the (id,
// tenant) it was asked for — backs the fatia 3 automatic task-creation tests.
func (m *mockRepo) GetActionItemCourtRecordID(_ context.Context, _ database.Tx, tenantID, actionItemID string) (string, error) {
	m.actionItemCourtRecordCalls++
	m.gotActionItemTenantID = tenantID
	m.gotActionItemCourtRecordID = actionItemID
	if m.actionItemCourtRecordIDErr != nil {
		return "", m.actionItemCourtRecordIDErr
	}
	return m.actionItemCourtRecordID, nil
}

func (m *mockRepo) GetLatestSuggestion(_ context.Context, _ database.Tx, _, _ string) (SuggestionRecord, bool, error) {
	m.latestSuggestionCalls++
	return m.latestSuggestion, m.latestSuggestionOK, m.latestSuggestionErr
}

func (m *mockRepo) GetDeadlineForCheck(_ context.Context, _ database.Tx, deadlineID, tenantID string) (*DeadlineForCheck, error) {
	m.checkCalls++
	m.gotCheckID = deadlineID
	m.gotCheckTenantID = tenantID
	return m.checkResult, m.checkErr
}

func (m *mockRepo) GetDeadlineEndDate(_ context.Context, _ database.Tx, deadlineID, tenantID string) (time.Time, error) {
	m.gotDeadlineEndDateID = deadlineID
	m.gotDeadlineEndDateTenant = tenantID
	m.deadlineEndDateCalls++
	return m.deadlineEndDate, m.deadlineEndDateErr
}

func (m *mockRepo) GetIntimationAssignee(_ context.Context, _ database.Tx, intimationID, _ string) (*string, error) {
	m.gotIntimationAssignID = intimationID
	m.intimationAssignCalls++
	return m.intimationAssignee, m.intimationAssigneeErr
}

func (m *mockRepo) MarkMissed(_ context.Context, _ database.Tx, deadlineID, tenantID string) (string, error) {
	m.markMissedCalls++
	m.gotMissedID = deadlineID
	m.gotMissedTenantID = tenantID
	return m.markMissedID, m.markMissedErr
}

// ListReconcilableDeadlines returns the configured MISSED/OPEN prazos and records the (record,
// tenant) scoping so a reconcile test can assert the 2-barrier key.
func (m *mockRepo) ListReconcilableDeadlines(_ context.Context, _ database.Tx, courtRecordID, tenantID string) ([]ReconcilableDeadline, error) {
	m.reconcilableCalls++
	m.gotReconcileRecord = courtRecordID
	m.gotReconcileTenant = tenantID
	return m.reconcilable, m.reconcilableErr
}

// HasResponseMovement returns the configured predicate result — a per-prazo override keyed by
// start_date when set, else the blanket hasResponse — and records the (record, tenant, tpuCodes)
// it was asked, so a test can assert the predicate scoping and the tpu code set.
func (m *mockRepo) HasResponseMovement(_ context.Context, _ database.Tx, courtRecordID, tenantID string, startDate time.Time, tpuCodes []int32) (bool, error) {
	m.hasResponseCalls++
	m.gotHasRespRecord = courtRecordID
	m.gotHasRespTenant = tenantID
	m.gotHasRespCodes = tpuCodes
	if m.hasResponseErr != nil {
		return false, m.hasResponseErr
	}
	if m.hasResponseByStart != nil {
		if v, ok := m.hasResponseByStart[startDate.Format(time.DateOnly)]; ok {
			return v, nil
		}
	}
	return m.hasResponse, nil
}

// MarkMet echoes the reconciled id (or a per-id/blanket error) and records every (id, tenant)
// it flipped, so a test can assert the guarded MET flip ran per matching prazo.
func (m *mockRepo) MarkMet(_ context.Context, _ database.Tx, deadlineID, tenantID string) (string, error) {
	m.markMetIDs = append(m.markMetIDs, deadlineID)
	m.markMetTenantIDs = append(m.markMetTenantIDs, tenantID)
	if m.markMetErrByID != nil {
		if err, ok := m.markMetErrByID[deadlineID]; ok {
			return "", err
		}
	}
	if m.markMetErr != nil {
		return "", m.markMetErr
	}
	if m.markMetID != "" {
		return m.markMetID, nil
	}
	return deadlineID, nil
}

func (m *mockRepo) GetDeadlineForConfirm(_ context.Context, _ database.Tx, intimationID, tenantID string) (*DeadlineForConfirm, error) {
	m.confirmAnchorCalls++
	m.gotConfirmIntimation = intimationID
	m.gotConfirmTenantID = tenantID
	return m.confirmAnchor, m.confirmAnchorErr
}

func (m *mockRepo) GetCourtRecordCourt(_ context.Context, _ database.Tx, tenantID, courtRecordID string) (string, error) {
	m.courtCalls++
	m.gotCourtTenantID = tenantID
	m.gotCourtRecordID = courtRecordID
	return m.courtRecordCourt, m.courtRecordErr
}

func (m *mockRepo) ConfirmDeadline(_ context.Context, _ database.Tx, p ConfirmDeadlineParams) (string, string, error) {
	m.confirmCalls++
	m.gotConfirmParams = p
	if m.confirmErr != nil {
		return "", "", m.confirmErr
	}
	return m.confirmID, m.confirmRecordID, nil
}

// ListTaskTitlesByDeadline models the confirm's read of the tasks that REALLY exist for the
// prazo (the feedback delta diffs the suggestion against these, not the confirm body). Returns
// the configured titles and records the (deadlineID, tenantID) scoping so a test can assert the
// 2-barrier key.
func (m *mockRepo) ListTaskTitlesByDeadline(_ context.Context, _ database.Tx, deadlineID, tenantID string) ([]string, error) {
	m.taskTitlesCalls++
	m.gotTitlesDeadlineID = deadlineID
	m.gotTitlesTenantID = tenantID
	return m.taskTitles, m.taskTitlesErr
}

// GetDeadlineForAdjust returns the configured adjustable state and records the (id, tenant)
// scoping so an ajuste test can assert the 2-barrier key.
func (m *mockRepo) GetDeadlineForAdjust(_ context.Context, _ database.Tx, deadlineID, tenantID string) (*DeadlineForAdjust, error) {
	m.adjustReadCalls++
	m.gotAdjustID = deadlineID
	m.gotAdjustTenantID = tenantID
	return m.adjustResult, m.adjustErr
}

// UpdateDeadlineAdjust captures the merged/recomputed params and echoes the configured ids,
// so a test can assert the patch was applied over the stored values and the recompute landed.
func (m *mockRepo) UpdateDeadlineAdjust(_ context.Context, _ database.Tx, p UpdateDeadlineAdjustParams) (string, string, error) {
	m.updateAdjustCalls++
	m.gotUpdateAdjustParams = p
	if m.updateAdjustErr != nil {
		return "", "", m.updateAdjustErr
	}
	return m.updateAdjustID, m.updateAdjustRecord, nil
}

// MarkDeadlineStatus records the (id, tenant, from, to) of a manual transition and returns the
// configured flipped id (or error), so a met/missed test can assert the guarded flip's args.
func (m *mockRepo) MarkDeadlineStatus(_ context.Context, _ database.Tx, deadlineID, tenantID string, from, to Status) (string, error) {
	m.markStatusCalls++
	m.gotMarkStatusID = deadlineID
	m.gotMarkStatusTenantID = tenantID
	m.gotMarkStatusFrom = from
	m.gotMarkStatusTo = to
	if m.markStatusErr != nil {
		return "", m.markStatusErr
	}
	return m.markStatusID, nil
}

// GetIntimationAnchors returns the configured anchors and records the (intimation, tenant)
// scoping so a re-anchor test can assert the 2-barrier key.
func (m *mockRepo) GetIntimationAnchors(_ context.Context, _ database.Tx, intimationID, tenantID string) (IntimationAnchors, error) {
	m.anchorsCalls++
	m.gotAnchorsIntim = intimationID
	m.gotAnchorsTenant = tenantID
	return m.anchors, m.anchorsErr
}

// GetPreviewContext returns the configured preview context (anchors + court) and records the
// (intimation, tenant) scoping.
func (m *mockRepo) GetPreviewContext(_ context.Context, _ database.Tx, intimationID, tenantID string) (PreviewContext, error) {
	m.previewContextCalls++
	m.gotPreviewIntim = intimationID
	m.gotPreviewTenant = tenantID
	return m.previewContext, m.previewContextErr
}

// MarkNoDeadline records the (id, tenant, confirmedBy, confirmedAt) of the mera-ciência flip and
// returns the configured id (or error), so a no-deadline test can assert the guarded flip's args.
func (m *mockRepo) MarkNoDeadline(_ context.Context, _ database.Tx, deadlineID, tenantID, confirmedBy string, confirmedAt time.Time) (string, error) {
	m.noDeadlineCalls++
	m.gotNoDeadlineID = deadlineID
	m.gotNoDeadlineTenant = tenantID
	m.gotNoDeadlineBy = confirmedBy
	m.gotNoDeadlineAt = confirmedAt
	if m.noDeadlineErr != nil {
		return "", m.noDeadlineErr
	}
	return m.noDeadlineID, nil
}

// ReopenNoDeadline records the (id, tenant) of the reopen flip and returns the configured id (or
// error).
func (m *mockRepo) ReopenNoDeadline(_ context.Context, _ database.Tx, deadlineID, tenantID string) (string, error) {
	m.reopenCalls++
	m.gotReopenID = deadlineID
	m.gotReopenTenant = tenantID
	if m.reopenErr != nil {
		return "", m.reopenErr
	}
	return m.reopenID, nil
}

// InsertTask echoes the task back with a real uuid id (as the DB would), so a test can
// assert task.created's aggregate is a parseable uuid, and captures every inserted task.
func (m *mockRepo) InsertTask(_ context.Context, _ database.Tx, t *Task) (*Task, error) {
	m.insertTaskCalls++
	if m.insertTaskErr != nil {
		return nil, m.insertTaskErr
	}
	saved := *t
	saved.ID = uuid.NewString()
	m.insertedTasks = append(m.insertedTasks, &saved)
	return &saved, nil
}

// GetTaskIDByActionItem returns the configured existing task id (the idempotent fallback the
// synchronous providência→tarefa path reads on an InsertTask conflict).
func (m *mockRepo) GetTaskIDByActionItem(_ context.Context, _ database.Tx, _, _ string) (string, error) {
	return m.taskIDByActionItem, m.taskIDByActionItemErr
}

// GetTaskForUpdate returns the configured editable state and records the (id, tenant) scoping so
// a PATCH test can assert the 2-barrier key.
func (m *mockRepo) GetTaskForUpdate(_ context.Context, _ database.Tx, taskID, tenantID string) (*TaskForUpdate, error) {
	m.taskForUpdateCalls++
	m.gotTaskUpdateID = taskID
	m.gotTaskUpdateTenantID = tenantID
	return m.taskForUpdate, m.taskForUpdateErr
}

// UpdateTask captures the merged params and echoes the configured saved task (or error), so a
// test can assert the patch was applied over the stored values.
func (m *mockRepo) UpdateTask(_ context.Context, _ database.Tx, p UpdateTaskParams) (*Task, error) {
	m.updateTaskCalls++
	m.gotUpdateTaskParams = p
	if m.updateTaskErr != nil {
		return nil, m.updateTaskErr
	}
	return m.updatedTask, nil
}

// GetTaskForTransition returns the configured status and records the (id, tenant) scoping so a
// done/dismiss test can assert the 2-barrier key.
func (m *mockRepo) GetTaskForTransition(_ context.Context, _ database.Tx, taskID, tenantID string) (TaskStatus, error) {
	m.taskTransitionCalls++
	m.gotTaskTransitionID = taskID
	m.gotTaskTransitionTID = tenantID
	return m.taskTransition, m.taskTransitionErr
}

// MarkTaskStatus records the (id, tenant, from, to, completed_at) of a manual task transition and
// returns the configured flipped id (or error), so a done/dismiss test can assert the guarded flip.
func (m *mockRepo) MarkTaskStatus(_ context.Context, _ database.Tx, taskID, tenantID string, from, to TaskStatus, completedAt *time.Time) (string, error) {
	m.markTaskStatusCalls++
	m.gotMarkTaskID = taskID
	m.gotMarkTaskTenantID = tenantID
	m.gotMarkTaskFrom = from
	m.gotMarkTaskTo = to
	m.gotMarkTaskCompleted = completedAt
	if m.markTaskStatusErr != nil {
		return "", m.markTaskStatusErr
	}
	return m.markTaskStatusID, nil
}

// EnsureTaskInTenant records the (task, tenant) guard scoping and returns the configured error
// (nil = the parent task exists), so a checklist-create test can assert the parent guard runs first.
func (m *mockRepo) EnsureTaskInTenant(_ context.Context, _ database.Tx, taskID, tenantID string) error {
	m.ensureTaskCalls++
	m.gotEnsureTaskID = taskID
	m.gotEnsureTenantID = tenantID
	return m.ensureTaskErr
}

// EnsureTaskExistsInTenant is the comment-create parent guard; returns the configured error.
func (m *mockRepo) EnsureTaskExistsInTenant(_ context.Context, _ database.Tx, _, _ string) error {
	return m.ensureTaskExistsErr
}

// InsertTaskComment records the inserted comment and echoes it back with an id + created_at.
func (m *mockRepo) InsertTaskComment(_ context.Context, _ database.Tx, c *TaskComment) (*TaskComment, error) {
	if m.insertCommentErr != nil {
		return nil, m.insertCommentErr
	}
	if m.insertedComment != nil {
		return m.insertedComment, nil
	}
	saved := *c
	saved.ID = uuid.NewString()
	saved.CreatedAt = time.Now()
	return &saved, nil
}

// InsertTaskActivity records every appended audit-log row so a test can assert the events written.
func (m *mockRepo) InsertTaskActivity(_ context.Context, _ database.Tx, a *TaskActivity) error {
	if m.insertActivityErr != nil {
		return m.insertActivityErr
	}
	m.insertedActivities = append(m.insertedActivities, a)
	return nil
}

// NextTaskItemPosition returns the configured append slot, recording the task it was asked for.
func (m *mockRepo) NextTaskItemPosition(_ context.Context, _ database.Tx, taskID, _ string) (int, error) {
	m.nextPosCalls++
	m.gotNextPosTaskID = taskID
	if m.nextItemPosErr != nil {
		return 0, m.nextItemPosErr
	}
	return m.nextItemPosition, nil
}

// InsertTaskItem records the inserted item and echoes back the configured saved item (with its id),
// mirroring how the DB would return the row.
func (m *mockRepo) InsertTaskItem(_ context.Context, _ database.Tx, item *TaskItem) (*TaskItem, error) {
	m.insertItemCalls++
	m.insertedItems = append(m.insertedItems, item)
	if m.insertItemErr != nil {
		return nil, m.insertItemErr
	}
	if m.insertedItem != nil {
		return m.insertedItem, nil
	}
	saved := *item
	saved.ID = uuid.NewString()
	return &saved, nil
}

// GetTaskItemForUpdate returns the configured editable state, recording the (item, task) scoping so
// a patch test can assert the cross-task guard key.
func (m *mockRepo) GetTaskItemForUpdate(_ context.Context, _ database.Tx, itemID, taskID, _ string) (*TaskItemForUpdate, error) {
	m.itemForUpdateCalls++
	m.gotItemForUpdateID = itemID
	m.gotItemForUpdateTask = taskID
	if m.itemForUpdateErr != nil {
		return nil, m.itemForUpdateErr
	}
	return m.itemForUpdate, nil
}

// UpdateTaskItem records the merged params (the caller-derived done_at included) and returns the
// configured saved item, so a patch test can assert the merge + done_at logic.
func (m *mockRepo) UpdateTaskItem(_ context.Context, _ database.Tx, p UpdateTaskItemParams) (*TaskItem, error) {
	m.updateItemCalls++
	m.gotUpdateItemParams = p
	if m.updateItemErr != nil {
		return nil, m.updateItemErr
	}
	return m.updatedItem, nil
}

// DeleteTaskItem records the (item, task, tenant) scoping and returns the configured error, so a
// delete test can assert the 404-on-miss and the barrier key.
func (m *mockRepo) DeleteTaskItem(_ context.Context, _ database.Tx, itemID, taskID, tenantID string) error {
	m.deleteItemCalls++
	m.gotDeleteItemID = itemID
	m.gotDeleteItemTask = taskID
	m.gotDeleteItemTenant = tenantID
	return m.deleteItemErr
}

// InsertCalcMemory is a V1 stub — captures the memory for assertion.
func (m *mockRepo) InsertCalcMemory(_ context.Context, _ database.Tx, cm *CalcMemory) (*CalcMemory, error) {
	m.gotCalcMemory = cm
	return cm, nil
}

// InsertAppliedHoliday is a V1 stub — captures each row for assertion (e.g. the
// name/âmbito the "por que essa data?" audit label resolves to).
func (m *mockRepo) InsertAppliedHoliday(_ context.Context, _ database.Tx, h *AppliedHoliday) (*AppliedHoliday, error) {
	m.gotAppliedHolidays = append(m.gotAppliedHolidays, h)
	return h, nil
}

// InsertCrossValidation is a V1 stub — captures the row for assertion.
func (m *mockRepo) InsertCrossValidation(_ context.Context, _ database.Tx, cv *CrossValidation) (*CrossValidation, error) {
	m.gotCrossValidation = cv
	return cv, nil
}

// InsertDeadlineEvent is a V1 stub — captures the event for assertion.
func (m *mockRepo) InsertDeadlineEvent(_ context.Context, _ database.Tx, e *DeadlineEvent) error {
	m.gotDeadlineEvent = e
	return nil
}

// GetPolicy is a V1 stub — returns the configured policy (default: seletiva).
func (m *mockRepo) GetPolicy(_ context.Context, _ database.Tx, tenantID string) (DeadlinePolicy, error) {
	return m.policy, nil
}

// GetCrossValidation is a V1 apuração stub (apurar.go) — returns the configured row/error.
func (m *mockRepo) GetCrossValidation(_ context.Context, _ database.Tx, tenantID, deadlineID string) (*CrossValidation, error) {
	m.gotGetCrossValidationTenant = tenantID
	m.gotGetCrossValidationID = deadlineID
	if m.crossValidationErr != nil {
		return nil, m.crossValidationErr
	}
	return m.crossValidation, nil
}

// UpdateCrossValidationDecision is a V1 apuração stub — captures the decision for assertion.
func (m *mockRepo) UpdateCrossValidationDecision(_ context.Context, _ database.Tx, tenantID, deadlineID, decisao, decididoPor string) error {
	m.updateCVDecisionCalls++
	m.gotUpdateCVDecisionTenant = tenantID
	m.gotUpdateCVDecisionID = deadlineID
	m.gotUpdateCVDecisionDecisao = decisao
	m.gotUpdateCVDecisionBy = decididoPor
	return m.updateCVDecisionErr
}

// UpdateDeadlineEndDate is a V1 apuração stub — captures the new end_date + prazo_interno for
// assertion.
func (m *mockRepo) UpdateDeadlineEndDate(_ context.Context, _ database.Tx, tenantID, deadlineID string, endDate, prazoInterno time.Time) error {
	m.updateEndDateCalls++
	m.gotUpdateEndDate = endDate
	m.gotUpdateEndDatePrazoInterno = prazoInterno
	return m.updateEndDateErr
}

// UpdateDeadlineSelo is a V1 apuração stub — captures the flipped selo + confirmed_by/at for
// assertion.
func (m *mockRepo) UpdateDeadlineSelo(_ context.Context, _ database.Tx, tenantID, deadlineID string, selo Seal, confirmedBy string, confirmedAt time.Time) error {
	m.updateSeloCalls++
	m.gotUpdateSelo = selo
	m.gotUpdateSeloConfirmedBy = confirmedBy
	m.gotUpdateSeloConfirmedAt = confirmedAt
	return m.updateSeloErr
}

// fakeCalendar records which motor was called (business vs calendar) and the args, and
// returns a configured end date + skipped days. The default end date is well after any
// start so validate() passes.
type fakeCalendar struct {
	businessCalls int
	calendarCalls int
	gotStart      time.Time
	gotN          int
	gotUF         string
	gotCourt      string
	endDate       time.Time
	holidays      []time.Time
	err           error

	// SubtractBusinessDays call recording/stubbing — the internal buffer's motor. When
	// subtractEndDate is zero, result() falls back to start.AddDate(0, 0, -n), mirroring
	// AddBusinessDays/AddCalendarDays' zero-value convention.
	subtractCalls   int
	gotSubtractArgs struct {
		start time.Time
		n     int
		uf    string
		court string
	}
	subtractEndDate time.Time
	subtractErr     error

	// LookupHolidays call recording/stubbing — the "por que essa data?" name/scope
	// lookup OnIntimationObserved uses to label applied_holiday.
	lookupCalls int
	lookupDays  []time.Time
	lookupUF    string
	lookupCourt string
	lookupLabel map[time.Time]calendar.Holiday
	lookupErr   error
}

func (c *fakeCalendar) LookupHolidays(_ context.Context, days []time.Time, uf, court string) (map[time.Time]calendar.Holiday, error) {
	c.lookupCalls++
	c.lookupDays, c.lookupUF, c.lookupCourt = days, uf, court
	if c.lookupErr != nil {
		return nil, c.lookupErr
	}
	return c.lookupLabel, nil
}

func (c *fakeCalendar) AddBusinessDays(_ context.Context, start time.Time, n int, uf, court string) (time.Time, []time.Time, error) {
	c.businessCalls++
	c.gotStart, c.gotN, c.gotUF, c.gotCourt = start, n, uf, court
	return c.result(start, n)
}

func (c *fakeCalendar) AddCalendarDays(_ context.Context, start time.Time, n int, uf, court string) (time.Time, []time.Time, error) {
	c.calendarCalls++
	c.gotStart, c.gotN, c.gotUF, c.gotCourt = start, n, uf, court
	return c.result(start, n)
}

func (c *fakeCalendar) result(start time.Time, n int) (time.Time, []time.Time, error) {
	if c.err != nil {
		return time.Time{}, nil, c.err
	}
	end := c.endDate
	if end.IsZero() {
		end = start.AddDate(0, 0, n)
	}
	return end, c.holidays, nil
}

// SubtractBusinessDays records the call and returns the configured subtractEndDate (or, when
// unset, start walked back n calendar days — a deterministic default so a test that never
// stubs the internal buffer still gets a stable, plausible date rather than a zero value).
func (c *fakeCalendar) SubtractBusinessDays(_ context.Context, start time.Time, n int, uf, court string) (time.Time, []time.Time, error) {
	c.subtractCalls++
	c.gotSubtractArgs.start, c.gotSubtractArgs.n, c.gotSubtractArgs.uf, c.gotSubtractArgs.court = start, n, uf, court
	if c.subtractErr != nil {
		return time.Time{}, nil, c.subtractErr
	}
	end := c.subtractEndDate
	if end.IsZero() {
		end = start.AddDate(0, 0, -n)
	}
	return end, nil, nil
}

// fakeUOW runs fn with a nil tx (the mocked repo/dedup never touch it) and records the
// RLS scope each Do asked for.
type fakeUOW struct {
	scopes []string
	err    error
}

func (u *fakeUOW) Do(_ context.Context, tenantID string, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, tenantID)
	if u.err != nil {
		return u.err
	}
	return fn(nil)
}

func (u *fakeUOW) DoSystem(_ context.Context, fn func(tx database.Tx) error) error {
	u.scopes = append(u.scopes, "system")
	return fn(nil)
}

// fakeDedup reports every event as first-seen by default; set seen=true to model an
// at-least-once replay.
type fakeDedup struct {
	seen      bool
	err       error
	marked    []string
	consumers []string
}

func (d *fakeDedup) SeenOrMark(_ context.Context, _ database.Tx, consumer, eventID string) (bool, error) {
	d.consumers = append(d.consumers, consumer)
	d.marked = append(d.marked, eventID)
	return d.seen, d.err
}

// fakeOutbox captures every published event so a test can assert the deadline.opened
// contract (aggregate, kind, end_date).
type fakeOutbox struct {
	published []events.Event
	err       error
}

func (o *fakeOutbox) Publish(_ context.Context, _ database.Tx, ev events.Event) error {
	o.published = append(o.published, ev)
	return o.err
}

// --- fixtures ---------------------------------------------------------------

// observedFixture builds a well-formed observed event for a cível record (TJSP/SP), the
// happy default; tests override the fields they exercise.
func observedFixture() IntimationObserved {
	return IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:        uuid.NewString(),
		IntimationID:    uuid.NewString(),
		CourtRecordID:   uuid.NewString(),
		CaseID:          uuid.NewString(),
		Type:            "CITACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2024-01-16",
	}
}

func citacaoRule() DeadlineRule {
	return DeadlineRule{RulesVersion: "v0", Kind: KindContestacao, Days: 15, Counting: CountingBusiness}
}

// cancelledFixture builds a well-formed intimation.cancelled, the revocation trigger;
// tests override the fields they exercise.
func cancelledFixture() IntimationCancelled {
	return IntimationCancelled{
		Base:         events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:     uuid.NewString(),
		IntimationID: uuid.NewString(),
		Reason:       "retificada pelo tribunal",
	}
}

// --- tests ------------------------------------------------------------------

// TestOnIntimationObserved_DerivesPendingRuleDeadline is the happy path: a CITACAO on a
// cível court derives a CONTESTACAO/15/BUSINESS prazo, born PENDING + source RULE +
// unconfirmed, computed via AddBusinessDays with the event's UF/court, and emits exactly
// one deadline.opened whose aggregate is the new deadline id.
func TestOnIntimationObserved_DerivesPendingRuleDeadline(t *testing.T) {
	ev := observedFixture()
	deadlineID := uuid.NewString()
	holiday := time.Date(2024, 1, 25, 0, 0, 0, 0, time.UTC)
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{class: "Procedimento Comum Cível", rule: citacaoRule(), insertID: deadlineID}
	cal := &fakeCalendar{endDate: end, holidays: []time.Time{holiday}}
	outbox := &fakeOutbox{}
	// Relógio ANTES do fim do prazo → nasce PENDING (o born-MISSED só vale depois da
	// carência D+1 — ver TestOnIntimationObserved_OverdueBornMissed).
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{},
		WithClock(func() time.Time { return time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC) }))

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	// The persisted prazo.
	d := repo.inserted
	if d == nil {
		t.Fatal("expected a deadline to be inserted")
	}
	if d.Status != StatusPending {
		t.Errorf("Status = %q, want PENDING (born as a suggestion)", d.Status)
	}
	if d.Source != SourceRule {
		t.Errorf("Source = %q, want RULE", d.Source)
	}
	if d.Kind != KindContestacao || d.Days != 15 || d.Counting != CountingBusiness {
		t.Errorf("kind/days/counting = %q/%d/%q, want CONTESTACAO/15/BUSINESS", d.Kind, d.Days, d.Counting)
	}
	if d.RulesVersion != "v0" {
		t.Errorf("RulesVersion = %q, want v0", d.RulesVersion)
	}
	if !d.EndDate.Equal(end) {
		t.Errorf("EndDate = %v, want %v", d.EndDate, end)
	}
	if len(d.HolidaysApplied) != 1 || !d.HolidaysApplied[0].Equal(holiday) {
		t.Errorf("HolidaysApplied = %v, want [%v]", d.HolidaysApplied, holiday)
	}
	if d.IntimationID != ev.IntimationID || d.CourtRecordID != ev.CourtRecordID || d.TenantID != ev.TenantID {
		t.Error("origin ids not carried onto the deadline")
	}

	// Cível → the dias-úteis motor, fed the event's UF/court (denormalized by producer).
	if cal.businessCalls != 1 || cal.calendarCalls != 0 {
		t.Errorf("calendar calls business=%d calendar=%d, want 1/0 (cível → dias úteis)", cal.businessCalls, cal.calendarCalls)
	}
	if cal.gotUF != "SP" || cal.gotCourt != "TJSP" || cal.gotN != 15 {
		t.Errorf("calendar args uf/court/n = %q/%q/%d, want SP/TJSP/15", cal.gotUF, cal.gotCourt, cal.gotN)
	}

	// The read used the event's record id (decisão P1), scoped to the event's tenant
	// (barrier 1 — the explicit tenant filter).
	if repo.gotClassRecordID != ev.CourtRecordID {
		t.Errorf("GetCourtRecordClass got %q, want %q", repo.gotClassRecordID, ev.CourtRecordID)
	}
	if repo.gotClassTenantID != ev.TenantID {
		t.Errorf("GetCourtRecordClass tenant = %q, want %q", repo.gotClassTenantID, ev.TenantID)
	}
	// The resolver got the event's type + court.
	if repo.gotRuleType != "CITACAO" || repo.gotRuleCourt != "TJSP" || repo.gotRuleVersion != rulesVersion {
		t.Errorf("ResolveRule got type/court/version = %q/%q/%q", repo.gotRuleType, repo.gotRuleCourt, repo.gotRuleVersion)
	}

	// Exactly one deadline.opened (o agendamento dos checks tem teste próprio), aggregate =
	// o novo deadline id (uuid parseável).
	openeds := publishedOfType[DeadlineOpened](outbox)
	if len(openeds) != 1 {
		t.Fatalf("deadline.opened publicados = %d, want 1", len(openeds))
	}
	opened := openeds[0]
	if opened.Type() != TypeDeadlineOpened || opened.AggregateType() != aggregateTypeDeadline {
		t.Errorf("event type/aggregate = %q/%q", opened.Type(), opened.AggregateType())
	}
	if opened.AggregateID() != deadlineID {
		t.Errorf("aggregate id = %q, want the deadline id %q", opened.AggregateID(), deadlineID)
	}
	if _, err := uuid.Parse(opened.AggregateID()); err != nil {
		t.Errorf("aggregate id is not a uuid: %v", err)
	}
	if opened.Kind != KindContestacao || opened.EndDate != "2024-02-06" || opened.Counting != "BUSINESS" {
		t.Errorf("opened kind/end/counting = %q/%q/%q", opened.Kind, opened.EndDate, opened.Counting)
	}
}

// TestOnIntimationObserved_PersistsPrazoInterno verifies the internal safety buffer is
// computed via SubtractBusinessDays(EndDate, internalBufferBusinessDays, uf, court) — the SAME
// event uf/court the forward AddBusinessDays motor used — and persisted on the deadline row
// (deadline.prazo_interno), not left as an in-memory read-time placeholder. The stubbed
// subtractEndDate lands well before a naive EndDate-minus-2-calendar-days would, standing in
// for a business-day count that crossed a holiday/weekend (the real crossing math is covered
// by lib/calendar's own SubtractBusinessDays tests).
func TestOnIntimationObserved_PersistsPrazoInterno(t *testing.T) {
	ev := observedFixture()
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)          // a Tuesday
	prazoInterno := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC) // 2 dias úteis antes, atravessando o fds

	repo := &mockRepo{class: "Procedimento Comum Cível", rule: citacaoRule(), insertID: uuid.NewString()}
	cal := &fakeCalendar{endDate: end, subtractEndDate: prazoInterno}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{},
		WithClock(func() time.Time { return time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC) }))

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	d := repo.inserted
	if d == nil {
		t.Fatal("expected a deadline to be inserted")
	}
	if !d.PrazoInterno.Equal(prazoInterno) {
		t.Errorf("PrazoInterno = %v, want %v", d.PrazoInterno, prazoInterno)
	}
	if cal.subtractCalls != 1 {
		t.Fatalf("SubtractBusinessDays calls = %d, want 1", cal.subtractCalls)
	}
	if !cal.gotSubtractArgs.start.Equal(end) {
		t.Errorf("SubtractBusinessDays start = %v, want the derived EndDate %v", cal.gotSubtractArgs.start, end)
	}
	if cal.gotSubtractArgs.n != internalBufferBusinessDays {
		t.Errorf("SubtractBusinessDays n = %d, want %d", cal.gotSubtractArgs.n, internalBufferBusinessDays)
	}
	if cal.gotSubtractArgs.uf != ev.UF || cal.gotSubtractArgs.court != ev.Court {
		t.Errorf("SubtractBusinessDays uf/court = %q/%q, want the event's %q/%q", cal.gotSubtractArgs.uf, cal.gotSubtractArgs.court, ev.UF, ev.Court)
	}
}

// TestOnIntimationObserved_ComunicacaoIsNoOp is the belt-and-suspenders guard: a generic
// COMUNICACAO opens no prazo, so it is dropped BEFORE any transaction — no dedup mark, no
// tx scope, no deadline inserted, no event published. The DJEN parser already gates these,
// so this protects the slice only against a future producer that emits one.
func TestOnIntimationObserved_ComunicacaoIsNoOp(t *testing.T) {
	ev := observedFixture()
	ev.Type = "COMUNICACAO"

	repo := &mockRepo{class: "Procedimento Comum Cível", rule: citacaoRule(), insertID: uuid.NewString()}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, uow)

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	if len(uow.scopes) != 0 {
		t.Errorf("opened %d transactions, want 0 (no-op before any tx)", len(uow.scopes))
	}
	if len(dedup.marked) != 0 {
		t.Errorf("dedup consulted %d times, want 0", len(dedup.marked))
	}
	if repo.inserted != nil {
		t.Error("a deadline was inserted for a COMUNICACAO, want none")
	}
	if len(outbox.published) != 0 {
		t.Errorf("published %d events, want 0", len(outbox.published))
	}
}

// TestOnIntimationObserved_OverdueBornMissed cobre o "prazo órfão" do backfill: quando a
// intimação é histórica e o prazo já NASCE vencido (a carência D+1 já passou no now da
// criação), ele nasce MISSED — não PENDING — senão o missed_check (agendado só para ETAs
// futuras em scheduleChecks) nunca é enfileirado e o prazo ficaria PENDING para sempre. É
// silencioso: emite só deadline.opened, nunca um deadline.missed nem checks agendados.
func TestOnIntimationObserved_OverdueBornMissed(t *testing.T) {
	ev := observedFixture()
	end := time.Date(2024, 2, 6, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC) // ~2 anos após o fim → carência passou

	repo := &mockRepo{class: "Procedimento Comum Cível", rule: citacaoRule(), insertID: uuid.NewString()}
	cal := &fakeCalendar{endDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{},
		WithClock(func() time.Time { return now }))

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	if repo.inserted == nil {
		t.Fatal("expected a deadline to be inserted")
	}
	if repo.inserted.Status != StatusMissed {
		t.Errorf("Status = %q, want MISSED (nascido já vencido)", repo.inserted.Status)
	}
	// Silencioso: opened + V1 calculated + V1 seal_assigned; nenhum deadline.missed,
	// missed_check ou reminder_check. (No confirmation required: seal=confiavel, policy=seletiva.)
	if got := len(outbox.published); got != 3 {
		t.Fatalf("published events = %d, want 3 (opened + calculated + seal_assigned)", got)
	}
	if _, ok := outbox.published[0].(DeadlineOpened); !ok {
		t.Errorf("published[0] = %T, want DeadlineOpened", outbox.published[0])
	}
	if n := len(publishedOfType[DeadlineMissedCheck](outbox)); n != 0 {
		t.Errorf("missed_check agendados = %d, want 0 (marca no passado)", n)
	}
	if n := len(publishedOfType[DeadlineReminderCheck](outbox)); n != 0 {
		t.Errorf("reminder_check agendados = %d, want 0 (marcas no passado)", n)
	}
}

// TestOnIntimationObserved_RuleToDeadline covers the rule-derived fields across the safe
// v0 rules the resolver returns (the resolution itself is SQL — here the mock stands in):
// each type maps to its kind/days, and an unknown type falls back to the GENERICO
// catch-all. Proves the domain builds the prazo straight from whatever the resolver gives.
func TestOnIntimationObserved_RuleToDeadline(t *testing.T) {
	tests := []struct {
		name     string
		typ      string
		rule     DeadlineRule
		wantKind string
		wantDays int
	}{
		{
			name:     "CITACAO → CONTESTACAO 15",
			typ:      "CITACAO",
			rule:     DeadlineRule{RulesVersion: "v0", Kind: KindContestacao, Days: 15, Counting: CountingBusiness},
			wantKind: KindContestacao,
			wantDays: 15,
		},
		{
			name:     "INTIMACAO → MANIFESTACAO 5",
			typ:      "INTIMACAO",
			rule:     DeadlineRule{RulesVersion: "v0", Kind: KindManifestacao, Days: 5, Counting: CountingBusiness},
			wantKind: KindManifestacao,
			wantDays: 5,
		},
		{
			name:     "unknown → GENERICO catch-all",
			typ:      "SOMETHING_ELSE",
			rule:     DeadlineRule{RulesVersion: "v0", Kind: KindGenerico, Days: 5, Counting: CountingBusiness},
			wantKind: KindGenerico,
			wantDays: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := observedFixture()
			ev.Type = tt.typ
			repo := &mockRepo{class: "Cível", rule: tt.rule, insertID: uuid.NewString()}
			uc := NewUseCase(repo, &fakeCalendar{}, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
				t.Fatalf("OnIntimationObserved() error = %v", err)
			}
			if repo.inserted.Kind != tt.wantKind || repo.inserted.Days != tt.wantDays {
				t.Errorf("kind/days = %q/%d, want %q/%d", repo.inserted.Kind, repo.inserted.Days, tt.wantKind, tt.wantDays)
			}
		})
	}
}

// TestOnIntimationObserved_LaborRiteUsesCalendarDays proves decisão P2: a labor court
// (TRT) overrides the rule's BUSINESS suggestion to CALENDAR (dias corridos), routing to
// AddCalendarDays and persisting counting=CALENDAR — even though the resolved rule
// suggested BUSINESS.
func TestOnIntimationObserved_LaborRiteUsesCalendarDays(t *testing.T) {
	ev := observedFixture()
	ev.Court = "TRT2"
	ev.UF = "SP"
	repo := &mockRepo{
		class:    "Reclamação Trabalhista",
		rule:     DeadlineRule{RulesVersion: "v0", Kind: KindGenerico, Days: 5, Counting: CountingBusiness},
		insertID: uuid.NewString(),
	}
	cal := &fakeCalendar{}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	if cal.calendarCalls != 1 || cal.businessCalls != 0 {
		t.Errorf("calendar calls calendar=%d business=%d, want 1/0 (labor → dias corridos)", cal.calendarCalls, cal.businessCalls)
	}
	if repo.inserted.Counting != CountingCalendar {
		t.Errorf("Counting = %q, want CALENDAR", repo.inserted.Counting)
	}
}

// TestOnIntimationObserved_Idempotent proves a replay (dedup reports seen) is a pure
// no-op: no rule read, no insert, no event — exactly one prazo can exist per event.
func TestOnIntimationObserved_Idempotent(t *testing.T) {
	ev := observedFixture()
	repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertID: uuid.NewString()}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}
	if repo.insertCalls != 0 {
		t.Errorf("InsertDeadline calls = %d, want 0 on a replay", repo.insertCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
	// It still dedups under the slice-specific consumer name.
	if len(dedup.consumers) != 1 || dedup.consumers[0] != consumerDeadline {
		t.Errorf("dedup consumer = %v, want [%q]", dedup.consumers, consumerDeadline)
	}
}

// TestOnIntimationObserved_ExistingDeadlineNoPhantom proves the 1:1 floor: when the
// insert reports ErrDeadlineExists (a prazo already exists for the intimação), the use
// case no-ops (returns nil, emits nothing) rather than opening a phantom prazo.
func TestOnIntimationObserved_ExistingDeadlineNoPhantom(t *testing.T) {
	ev := observedFixture()
	repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertErr: ErrDeadlineExists}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v, want nil (idempotent no-op)", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 (no phantom deadline.opened)", len(outbox.published))
	}
}

// TestOnIntimationObserved_Errors proves the infra errors propagate (retryable) and the
// malformed anchor is a terminal invalid — each aborts before any event is published.
func TestOnIntimationObserved_Errors(t *testing.T) {
	infra := errors.New("boom")
	tests := []struct {
		name     string
		mutate   func(ev *IntimationObserved, r *mockRepo)
		wantKind apperr.Kind
		wantErr  bool
	}{
		{
			name:     "malformed anchor → invalid",
			mutate:   func(ev *IntimationObserved, _ *mockRepo) { ev.DeadlineStartAt = "not-a-date" },
			wantKind: apperr.KindInvalid,
			wantErr:  true,
		},
		{
			name:    "court_record read fails → propagate",
			mutate:  func(_ *IntimationObserved, r *mockRepo) { r.classErr = infra },
			wantErr: true,
		},
		{
			name:    "rule resolve fails → propagate",
			mutate:  func(_ *IntimationObserved, r *mockRepo) { r.ruleErr = ErrRuleNotFound },
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := observedFixture()
			repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertID: uuid.NewString()}
			tt.mutate(&ev, repo)
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			err := uc.OnIntimationObserved(context.Background(), ev)
			if tt.wantErr && err == nil {
				t.Fatal("expected an error")
			}
			if tt.wantKind != "" {
				ae, ok := apperr.From(err)
				if !ok || ae.Kind != tt.wantKind {
					t.Errorf("error kind = %v, want %q", err, tt.wantKind)
				}
			}
			if len(outbox.published) != 0 {
				t.Errorf("published events = %d, want 0 on error", len(outbox.published))
			}
		})
	}
}

// publishedOfType returns every published event of the concrete type T — the reminder/miss
// marks and lembretes are asserted per-type without index bookkeeping.
func publishedOfType[T events.Event](out *fakeOutbox) []T {
	var res []T
	for _, ev := range out.published {
		if v, ok := ev.(T); ok {
			res = append(res, v)
		}
	}
	return res
}

// TestOnIntimationObserved_SchedulesReminderAndMissedChecks proves the 4b-ii creation
// extension: after the deadline.opened, a fresh prazo whose end_date is comfortably in the
// future schedules all three D-N reminder_check marks (days_left 3/1/0, each with the right
// ETA and stable idempotency key) plus one missed_check at D+1 — every mark carrying the
// tenant and the deadline id as aggregate.
func TestOnIntimationObserved_SchedulesReminderAndMissedChecks(t *testing.T) {
	ev := observedFixture()
	deadlineID := uuid.NewString()
	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)

	repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertID: deadlineID}
	cal := &fakeCalendar{endDate: end}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{}, WithClock(func() time.Time { return now }))

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	// deadline.opened is still emitted exactly once (the marks are additive).
	if got := len(publishedOfType[DeadlineOpened](outbox)); got != 1 {
		t.Fatalf("deadline.opened events = %d, want 1", got)
	}

	// Three reminder_check marks: days_left 3/1/0, ETA = start-of-day(end) − days_left.
	reminders := publishedOfType[DeadlineReminderCheck](outbox)
	if len(reminders) != 3 {
		t.Fatalf("reminder_check marks = %d, want 3", len(reminders))
	}
	wantAt := map[int]time.Time{
		3: time.Date(2024, 1, 29, 0, 0, 0, 0, time.UTC),
		1: time.Date(2024, 1, 31, 0, 0, 0, 0, time.UTC),
		0: end,
	}
	seen := map[int]bool{}
	for _, r := range reminders {
		seen[r.DaysLeft] = true
		at, ok := r.ProcessAt()
		if !ok || !at.Equal(wantAt[r.DaysLeft]) {
			t.Errorf("reminder days_left=%d ProcessAt = %v (ok=%v), want %v", r.DaysLeft, at, ok, wantAt[r.DaysLeft])
		}
		if r.TenantID != ev.TenantID {
			t.Errorf("reminder days_left=%d tenant = %q, want %q", r.DaysLeft, r.TenantID, ev.TenantID)
		}
		if r.AggregateType() != aggregateTypeDeadline || r.AggregateID() != deadlineID {
			t.Errorf("reminder aggregate = %q/%q, want deadline/%q", r.AggregateType(), r.AggregateID(), deadlineID)
		}
		if want := fmt.Sprintf("deadline-reminder:%s:%d", deadlineID, r.DaysLeft); r.IdempotencyKey() != want {
			t.Errorf("reminder idempotency key = %q, want %q", r.IdempotencyKey(), want)
		}
	}
	for _, dl := range reminderDaysLeft {
		if !seen[dl] {
			t.Errorf("missing reminder_check for days_left=%d", dl)
		}
	}

	// One missed_check at D+1, stable key, tenant + aggregate carried.
	missed := publishedOfType[DeadlineMissedCheck](outbox)
	if len(missed) != 1 {
		t.Fatalf("missed_check marks = %d, want 1", len(missed))
	}
	at, ok := missed[0].ProcessAt()
	if !ok || !at.Equal(time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("missed_check ProcessAt = %v (ok=%v), want 2024-02-02", at, ok)
	}
	if missed[0].TenantID != ev.TenantID || missed[0].AggregateID() != deadlineID {
		t.Errorf("missed_check tenant/aggregate = %q/%q", missed[0].TenantID, missed[0].AggregateID())
	}
	if want := "deadline-missed:" + deadlineID; missed[0].IdempotencyKey() != want {
		t.Errorf("missed_check idempotency key = %q, want %q", missed[0].IdempotencyKey(), want)
	}

	// opened + 3 reminders + 1 missed + V1 calculated + V1 seal_assigned = 7 total, nothing else.
	if len(outbox.published) != 7 {
		t.Errorf("total published = %d, want 7", len(outbox.published))
	}
}

// TestOnIntimationObserved_SkipsPastMarks proves the birth-time future-guard: a mark whose
// ETA already passed at birth is never scheduled. A prazo born <3 dias do fim skips D-3; a
// prazo born after its vencimento schedules nothing at all.
func TestOnIntimationObserved_SkipsPastMarks(t *testing.T) {
	end := time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		now          time.Time
		wantDaysLeft []int
		wantMissed   bool
		wantTotalPub int
	}{
		{
			name:         "born <3 days before end skips D-3",
			now:          time.Date(2024, 1, 30, 12, 0, 0, 0, time.UTC), // D-3 (01-29) already past
			wantDaysLeft: []int{1, 0},
			wantMissed:   true,
			wantTotalPub: 6, // opened + 2 reminders + missed + V1 calculated + V1 seal_assigned
		},
		{
			name:         "born after vencimento schedules nothing",
			now:          time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC), // even D+1 is past
			wantDaysLeft: nil,
			wantMissed:   false,
			wantTotalPub: 3, // opened + V1 calculated + V1 seal_assigned
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := observedFixture()
			repo := &mockRepo{class: "Cível", rule: citacaoRule(), insertID: uuid.NewString()}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{endDate: end}, outbox, &fakeDedup{}, &fakeUOW{},
				WithClock(func() time.Time { return tt.now }))

			if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
				t.Fatalf("OnIntimationObserved() error = %v", err)
			}

			reminders := publishedOfType[DeadlineReminderCheck](outbox)
			if len(reminders) != len(tt.wantDaysLeft) {
				t.Fatalf("reminder marks = %d, want %d", len(reminders), len(tt.wantDaysLeft))
			}
			got := map[int]bool{}
			for _, r := range reminders {
				got[r.DaysLeft] = true
			}
			for _, dl := range tt.wantDaysLeft {
				if !got[dl] {
					t.Errorf("missing reminder for days_left=%d", dl)
				}
			}
			if gotMissed := len(publishedOfType[DeadlineMissedCheck](outbox)) == 1; gotMissed != tt.wantMissed {
				t.Errorf("missed_check scheduled = %v, want %v", gotMissed, tt.wantMissed)
			}
			if len(outbox.published) != tt.wantTotalPub {
				t.Errorf("total published = %d, want %d", len(outbox.published), tt.wantTotalPub)
			}
		})
	}
}

// TestOnIntimationCancelled_RevokesExistingDeadline is the revocation happy path: the
// repo reports the prazo was flipped to CANCELLED, so the use case emits exactly one
// deadline.revoked whose aggregate is the revoked deadline id (a parseable uuid) and whose
// payload carries the triggering intimação + the DJEN reason. The revoke is scoped to the
// event's tenant/intimação (barrier 1 args threaded through).
func TestOnIntimationCancelled_RevokesExistingDeadline(t *testing.T) {
	ev := cancelledFixture()
	deadlineID := uuid.NewString()
	courtRecordID := uuid.NewString()

	repo := &mockRepo{revokeResult: &RevokedDeadline{ID: deadlineID, CourtRecordID: courtRecordID}}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnIntimationCancelled(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationCancelled() error = %v", err)
	}

	// The revoke read the event's intimação, scoped to the event's tenant (barrier 1).
	if repo.revokeCalls != 1 {
		t.Fatalf("RevokeDeadlineByIntimation calls = %d, want 1", repo.revokeCalls)
	}
	if repo.gotRevokeIntimationID != ev.IntimationID || repo.gotRevokeTenantID != ev.TenantID {
		t.Errorf("revoke args intimation/tenant = %q/%q, want %q/%q",
			repo.gotRevokeIntimationID, repo.gotRevokeTenantID, ev.IntimationID, ev.TenantID)
	}

	// Exactly one deadline.revoked, aggregate = the revoked deadline id (a uuid).
	if len(outbox.published) != 1 {
		t.Fatalf("published events = %d, want 1", len(outbox.published))
	}
	revoked, ok := outbox.published[0].(DeadlineRevoked)
	if !ok {
		t.Fatalf("published[0] type = %T, want DeadlineRevoked", outbox.published[0])
	}
	if revoked.Type() != TypeDeadlineRevoked || revoked.AggregateType() != aggregateTypeDeadline {
		t.Errorf("event type/aggregate = %q/%q", revoked.Type(), revoked.AggregateType())
	}
	if revoked.AggregateID() != deadlineID {
		t.Errorf("aggregate id = %q, want the deadline id %q", revoked.AggregateID(), deadlineID)
	}
	if _, err := uuid.Parse(revoked.AggregateID()); err != nil {
		t.Errorf("aggregate id is not a uuid: %v", err)
	}
	if revoked.DeadlineID != deadlineID || revoked.IntimationID != ev.IntimationID || revoked.Reason != ev.Reason {
		t.Errorf("revoked deadline/intimation/reason = %q/%q/%q, want %q/%q/%q",
			revoked.DeadlineID, revoked.IntimationID, revoked.Reason, deadlineID, ev.IntimationID, ev.Reason)
	}
}

// TestOnIntimationCancelled_NoDeadlineNoOp proves the safe bias: when there is no prazo to
// revoke — none was ever derived (the cancel raced ahead of the observe, or the intimação
// was dead on arrival) OR it is already CANCELLED — the repo returns ErrDeadlineNotFound
// and the use case no-ops: no error, no phantom deadline.revoked. Both collapse to the
// same path because the query's status <> CANCELLED guard touches no row in either case.
func TestOnIntimationCancelled_NoDeadlineNoOp(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "no prazo exists for the intimação"},
		{name: "prazo already CANCELLED"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := cancelledFixture()
			repo := &mockRepo{revokeErr: ErrDeadlineNotFound}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnIntimationCancelled(context.Background(), ev); err != nil {
				t.Fatalf("OnIntimationCancelled() error = %v, want nil (safe no-op)", err)
			}
			if repo.revokeCalls != 1 {
				t.Errorf("RevokeDeadlineByIntimation calls = %d, want 1", repo.revokeCalls)
			}
			if len(outbox.published) != 0 {
				t.Errorf("published events = %d, want 0 (no phantom deadline.revoked)", len(outbox.published))
			}
		})
	}
}

// TestOnIntimationCancelled_Idempotent proves a replay (dedup reports seen) is a pure
// no-op: no revoke, no event — the dedup floor guards the revocation exactly as it guards
// the creation path.
func TestOnIntimationCancelled_Idempotent(t *testing.T) {
	ev := cancelledFixture()
	repo := &mockRepo{revokeResult: &RevokedDeadline{ID: uuid.NewString(), CourtRecordID: uuid.NewString()}}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnIntimationCancelled(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationCancelled() error = %v", err)
	}
	if repo.revokeCalls != 0 {
		t.Errorf("RevokeDeadlineByIntimation calls = %d, want 0 on a replay", repo.revokeCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
	// It still dedups under the slice-specific consumer name.
	if len(dedup.consumers) != 1 || dedup.consumers[0] != consumerDeadline {
		t.Errorf("dedup consumer = %v, want [%q]", dedup.consumers, consumerDeadline)
	}
}

// TestOnIntimationCancelled_InfraErrorPropagates proves an infra fault from the revoke
// aborts the tx (retryable) and emits nothing — only ErrDeadlineNotFound is a no-op.
func TestOnIntimationCancelled_InfraErrorPropagates(t *testing.T) {
	ev := cancelledFixture()
	repo := &mockRepo{revokeErr: apperr.NewInfra("db down", errors.New("boom"))}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	err := uc.OnIntimationCancelled(context.Background(), ev)
	if err == nil {
		t.Fatal("expected an error to propagate")
	}
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInfra {
		t.Errorf("error kind = %v, want KindInfra", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on error", len(outbox.published))
	}
}

// docketFixture builds a well-formed docket_entry_observed for the reconcile path; tests
// override the fields they exercise.
func docketFixture() DocketEntryObserved {
	return DocketEntryObserved{
		Base:          events.Base{EventID: uuid.NewString(), Aggregate: uuid.NewString()},
		TenantID:      uuid.NewString(),
		CourtRecordID: uuid.NewString(),
	}
}

// TestOnDocketEntryObserved_MissedReconciledToMet is case (a): a MISSED prazo whose
// court_record holds a response movement after its start_date is flipped to MET and emits
// exactly one deadline.met (aggregate = the reconciled deadline id). The predicate got the
// event's record/tenant + the response TPU code set.
func TestOnDocketEntryObserved_MissedReconciledToMet(t *testing.T) {
	ev := docketFixture()
	deadlineID := uuid.NewString()
	start := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		reconcilable: []ReconcilableDeadline{{ID: deadlineID, StartDate: start}},
		hasResponse:  true,
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnDocketEntryObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnDocketEntryObserved() error = %v", err)
	}

	// The list + predicate ran scoped to the event's record/tenant (barrier 1).
	if repo.gotReconcileRecord != ev.CourtRecordID || repo.gotReconcileTenant != ev.TenantID {
		t.Errorf("list scope record/tenant = %q/%q, want %q/%q",
			repo.gotReconcileRecord, repo.gotReconcileTenant, ev.CourtRecordID, ev.TenantID)
	}
	if repo.gotHasRespRecord != ev.CourtRecordID || repo.gotHasRespTenant != ev.TenantID {
		t.Errorf("predicate scope record/tenant = %q/%q", repo.gotHasRespRecord, repo.gotHasRespTenant)
	}
	// The predicate carries the fixed response TPU code set.
	if len(repo.gotHasRespCodes) != len(responseTPUCodes) {
		t.Errorf("tpu codes = %v, want %v", repo.gotHasRespCodes, responseTPUCodes)
	}
	// The prazo was flipped MET.
	if len(repo.markMetIDs) != 1 || repo.markMetIDs[0] != deadlineID {
		t.Errorf("MarkMet ids = %v, want [%q]", repo.markMetIDs, deadlineID)
	}
	if repo.markMetTenantIDs[0] != ev.TenantID {
		t.Errorf("MarkMet tenant = %q, want %q", repo.markMetTenantIDs[0], ev.TenantID)
	}

	// Exactly one deadline.met, aggregate = the reconciled deadline id (a uuid).
	mets := publishedOfType[DeadlineMet](outbox)
	if len(mets) != 1 {
		t.Fatalf("deadline.met events = %d, want 1", len(mets))
	}
	met := mets[0]
	if met.Type() != TypeDeadlineMet || met.AggregateType() != aggregateTypeDeadline {
		t.Errorf("event type/aggregate = %q/%q", met.Type(), met.AggregateType())
	}
	if met.DeadlineID != deadlineID || met.TenantID != ev.TenantID {
		t.Errorf("met deadline/tenant = %q/%q, want %q/%q", met.DeadlineID, met.TenantID, deadlineID, ev.TenantID)
	}
	if _, err := uuid.Parse(met.AggregateID()); err != nil {
		t.Errorf("aggregate id is not a uuid: %v", err)
	}
}

// TestOnDocketEntryObserved_OpenReconciledToMet is case (b): an OPEN prazo (not just MISSED)
// with a response movement is likewise flipped to MET + emits deadline.met — the reconcile
// resurrects both reconcilable statuses.
func TestOnDocketEntryObserved_OpenReconciledToMet(t *testing.T) {
	ev := docketFixture()
	deadlineID := uuid.NewString()

	// The mock does not model status (the guarded MarkMet does), but the caller only lists
	// MISSED/OPEN — so an OPEN prazo reaching MarkMet is exactly this case.
	repo := &mockRepo{
		reconcilable: []ReconcilableDeadline{{ID: deadlineID, StartDate: time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)}},
		hasResponse:  true,
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnDocketEntryObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnDocketEntryObserved() error = %v", err)
	}
	if len(repo.markMetIDs) != 1 || repo.markMetIDs[0] != deadlineID {
		t.Errorf("MarkMet ids = %v, want [%q]", repo.markMetIDs, deadlineID)
	}
	if got := len(publishedOfType[DeadlineMet](outbox)); got != 1 {
		t.Errorf("deadline.met events = %d, want 1", got)
	}
}

// TestOnDocketEntryObserved_NoResponseNoOp is case (c): a reconcilable prazo with NO response
// movement is left untouched — no MarkMet, no deadline.met.
func TestOnDocketEntryObserved_NoResponseNoOp(t *testing.T) {
	ev := docketFixture()
	repo := &mockRepo{
		reconcilable: []ReconcilableDeadline{{ID: uuid.NewString(), StartDate: time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)}},
		hasResponse:  false,
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnDocketEntryObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnDocketEntryObserved() error = %v", err)
	}
	if repo.hasResponseCalls != 1 {
		t.Errorf("HasResponseMovement calls = %d, want 1", repo.hasResponseCalls)
	}
	if len(repo.markMetIDs) != 0 {
		t.Errorf("MarkMet calls = %v, want none (no response)", repo.markMetIDs)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 (no response)", len(outbox.published))
	}
}

// TestOnDocketEntryObserved_MarkMetGuardNoOp is case (d): the guarded MarkMet touches no row
// (a racing flip already moved the prazo out of MISSED/OPEN → ErrDeadlineNotFound). The
// reconcile treats it as a per-prazo no-op: no deadline.met, no error, keeps going.
func TestOnDocketEntryObserved_MarkMetGuardNoOp(t *testing.T) {
	ev := docketFixture()
	racedID := uuid.NewString()
	repo := &mockRepo{
		reconcilable:   []ReconcilableDeadline{{ID: racedID, StartDate: time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)}},
		hasResponse:    true,
		markMetErrByID: map[string]error{racedID: ErrDeadlineNotFound},
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnDocketEntryObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnDocketEntryObserved() error = %v, want nil (guarded no-op)", err)
	}
	if len(repo.markMetIDs) != 1 {
		t.Errorf("MarkMet calls = %d, want 1 (attempted the flip)", len(repo.markMetIDs))
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 (0 rows flipped)", len(outbox.published))
	}
}

// TestOnDocketEntryObserved_NoReconcilableNoOp is case (e): a court_record with NO MISSED/OPEN
// prazo (all PENDING/MET/CANCELLED) reconciles nothing — the predicate is never even asked.
// The MISSED/OPEN-only filter lives in the query (ListReconcilableDeadlines), so a PENDING or
// CANCELLED prazo simply never appears in the reconcilable set the use case iterates.
func TestOnDocketEntryObserved_NoReconcilableNoOp(t *testing.T) {
	ev := docketFixture()
	repo := &mockRepo{reconcilable: nil} // no MISSED/OPEN prazo for the record
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnDocketEntryObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnDocketEntryObserved() error = %v", err)
	}
	if repo.reconcilableCalls != 1 {
		t.Errorf("ListReconcilableDeadlines calls = %d, want 1", repo.reconcilableCalls)
	}
	if repo.hasResponseCalls != 0 {
		t.Errorf("HasResponseMovement calls = %d, want 0 (nothing to reconcile)", repo.hasResponseCalls)
	}
	if len(repo.markMetIDs) != 0 || len(outbox.published) != 0 {
		t.Errorf("MarkMet/published = %v/%d, want none", repo.markMetIDs, len(outbox.published))
	}
}

// TestOnDocketEntryObserved_Idempotent is case (f): a replay (dedup reports seen) is a pure
// no-op — no list, no predicate, no flip, no event. It dedups under the SEPARATE
// consumerReconcile name (NOT consumerDeadline), so the reconcile has its own
// processed_event floor independent of the creation/revocation consumer.
func TestOnDocketEntryObserved_Idempotent(t *testing.T) {
	ev := docketFixture()
	repo := &mockRepo{
		reconcilable: []ReconcilableDeadline{{ID: uuid.NewString(), StartDate: time.Now()}},
		hasResponse:  true,
	}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnDocketEntryObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnDocketEntryObserved() error = %v", err)
	}
	if repo.reconcilableCalls != 0 {
		t.Errorf("ListReconcilableDeadlines calls = %d, want 0 on a replay", repo.reconcilableCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
	// It dedups under the reconcile-specific consumer name, NOT the deadline creation one.
	if len(dedup.consumers) != 1 || dedup.consumers[0] != consumerReconcile {
		t.Errorf("dedup consumer = %v, want [%q]", dedup.consumers, consumerReconcile)
	}
	if consumerReconcile == consumerDeadline {
		t.Error("reconcile must dedup under a DISTINCT consumer name from the creation path")
	}
}

// TestOnDocketEntryObserved_MultiplePrazos proves the reconcile re-checks EVERY MISSED/OPEN
// prazo of the court_record (not just one): given two reconcilable prazos where only one has a
// response movement (per-start_date predicate override), exactly that one is flipped to MET and
// emits deadline.met — the other is left untouched.
func TestOnDocketEntryObserved_MultiplePrazos(t *testing.T) {
	ev := docketFixture()
	metID := uuid.NewString()
	keepID := uuid.NewString()
	metStart := time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)
	keepStart := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		reconcilable: []ReconcilableDeadline{
			{ID: metID, StartDate: metStart},
			{ID: keepID, StartDate: keepStart},
		},
		// Only the earlier prazo has a response movement on/after its start_date.
		hasResponseByStart: map[string]bool{
			metStart.Format(time.DateOnly):  true,
			keepStart.Format(time.DateOnly): false,
		},
	}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	if err := uc.OnDocketEntryObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnDocketEntryObserved() error = %v", err)
	}
	if repo.hasResponseCalls != 2 {
		t.Errorf("HasResponseMovement calls = %d, want 2 (both prazos re-checked)", repo.hasResponseCalls)
	}
	if len(repo.markMetIDs) != 1 || repo.markMetIDs[0] != metID {
		t.Errorf("MarkMet ids = %v, want only [%q]", repo.markMetIDs, metID)
	}
	mets := publishedOfType[DeadlineMet](outbox)
	if len(mets) != 1 || mets[0].DeadlineID != metID {
		t.Errorf("deadline.met = %v, want one for %q", mets, metID)
	}
}

// TestOnDocketEntryObserved_InfraErrorPropagates proves an infra fault (from the list or the
// predicate) aborts the tx (retryable) and emits nothing — only ErrDeadlineNotFound from
// MarkMet is a per-prazo no-op.
func TestOnDocketEntryObserved_InfraErrorPropagates(t *testing.T) {
	infra := apperr.NewInfra("db down", errors.New("boom"))
	tests := []struct {
		name   string
		mutate func(r *mockRepo)
	}{
		{
			name:   "list fails → propagate",
			mutate: func(r *mockRepo) { r.reconcilableErr = infra },
		},
		{
			name: "predicate fails → propagate",
			mutate: func(r *mockRepo) {
				r.reconcilable = []ReconcilableDeadline{{ID: uuid.NewString(), StartDate: time.Now()}}
				r.hasResponseErr = infra
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := docketFixture()
			repo := &mockRepo{}
			tt.mutate(repo)
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			err := uc.OnDocketEntryObserved(context.Background(), ev)
			if err == nil {
				t.Fatal("expected an error to propagate")
			}
			ae, ok := apperr.From(err)
			if !ok || ae.Kind != apperr.KindInfra {
				t.Errorf("error kind = %v, want KindInfra", err)
			}
			if len(outbox.published) != 0 {
				t.Errorf("published events = %d, want 0 on error", len(outbox.published))
			}
		})
	}
}

// TestDecideCounting exercises the pure rito decision (P2): only a positive labor signal
// (a labor court sigla, or a class naming the rito) flips the rule's suggestion to
// CALENDAR; anything else keeps the conservative suggestion (viés seguro).
func TestDecideCounting(t *testing.T) {
	tests := []struct {
		name      string
		suggested Counting
		court     string
		class     string
		want      Counting
	}{
		{"cível keeps business", CountingBusiness, "TJSP", "Procedimento Comum", CountingBusiness},
		{"TRT court → calendar", CountingBusiness, "TRT2", "", CountingCalendar},
		{"TST court → calendar", CountingBusiness, "TST", "", CountingCalendar},
		{"class names trabalhista → calendar", CountingBusiness, "TJSP", "Ação Trabalhista", CountingCalendar},
		{"unknown court keeps suggestion", CountingBusiness, "TJRJ", "Cível", CountingBusiness},
		{"rule already calendar is kept", CountingCalendar, "TJSP", "", CountingCalendar},
		{"empty court keeps suggestion", CountingBusiness, "", "", CountingBusiness},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideCounting(tt.suggested, tt.court, tt.class); got != tt.want {
				t.Errorf("decideCounting(%q, %q, %q) = %q, want %q", tt.suggested, tt.court, tt.class, got, tt.want)
			}
		})
	}
}

// TestParseWireDate covers the anchor parse: the wire format succeeds, anything else is
// a terminal KindInvalid.
func TestParseWireDate(t *testing.T) {
	got, err := parseWireDate("2024-01-16")
	if err != nil {
		t.Fatalf("parseWireDate valid: %v", err)
	}
	if !got.Equal(time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("parsed = %v, want 2024-01-16 UTC", got)
	}

	_, err = parseWireDate("16/01/2024")
	ae, ok := apperr.From(err)
	if !ok || ae.Kind != apperr.KindInvalid {
		t.Errorf("parseWireDate invalid = %v, want KindInvalid", err)
	}
}

// reminderCheckFixture builds a fired reminder_check for a given prazo id/tenant/days_left.
// processAt is irrelevant on the fire path (the handler re-loads by id), so it stays zero.
func reminderCheckFixture(tenantID, deadlineID string, daysLeft int) DeadlineReminderCheck {
	return DeadlineReminderCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   tenantID,
		DeadlineID: deadlineID,
		DaysLeft:   daysLeft,
	}
}

// TestOnReminderCheck_ReChecksStatus proves the re-check no disparo (decisão travada:
// due_soon avisa OPEN e PENDING, viés seguro): a fired reminder emits exactly one
// deadline.due_soon only when the prazo is still PENDING or OPEN, and is a pure no-op for
// any terminal status (a prazo cancelled/met/missed between birth and the mark never sends
// a stale lembrete).
func TestOnReminderCheck_ReChecksStatus(t *testing.T) {
	tests := []struct {
		name     string
		status   Status
		wantEmit bool
	}{
		{"PENDING → due_soon", StatusPending, true},
		{"OPEN → due_soon", StatusOpen, true},
		{"CANCELLED → no-op", StatusCancelled, false},
		{"MET → no-op", StatusMet, false},
		{"MISSED → no-op", StatusMissed, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deadlineID := uuid.NewString()
			ev := reminderCheckFixture(uuid.NewString(), deadlineID, 3)
			repo := &mockRepo{checkResult: &DeadlineForCheck{ID: deadlineID, Status: tt.status}}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnReminderCheck(context.Background(), ev); err != nil {
				t.Fatalf("OnReminderCheck() error = %v", err)
			}

			// The re-read was tenant-scoped to the event (barrier 1 args threaded).
			if repo.checkCalls != 1 || repo.gotCheckID != deadlineID || repo.gotCheckTenantID != ev.TenantID {
				t.Errorf("GetDeadlineForCheck calls/id/tenant = %d/%q/%q, want 1/%q/%q",
					repo.checkCalls, repo.gotCheckID, repo.gotCheckTenantID, deadlineID, ev.TenantID)
			}

			dueSoon := publishedOfType[DeadlineDueSoon](outbox)
			if !tt.wantEmit {
				if len(dueSoon) != 0 {
					t.Fatalf("due_soon events = %d, want 0 (obsolete mark)", len(dueSoon))
				}
				return
			}
			if len(dueSoon) != 1 {
				t.Fatalf("due_soon events = %d, want 1", len(dueSoon))
			}
			d := dueSoon[0]
			if d.Type() != TypeDeadlineDueSoon || d.AggregateType() != aggregateTypeDeadline {
				t.Errorf("due_soon type/aggregate = %q/%q", d.Type(), d.AggregateType())
			}
			if d.AggregateID() != deadlineID {
				t.Errorf("due_soon aggregate id = %q, want %q", d.AggregateID(), deadlineID)
			}
			if _, err := uuid.Parse(d.AggregateID()); err != nil {
				t.Errorf("due_soon aggregate id is not a uuid: %v", err)
			}
			if d.DeadlineID != deadlineID || d.DaysLeft != ev.DaysLeft || d.TenantID != ev.TenantID {
				t.Errorf("due_soon deadline/days/tenant = %q/%d/%q, want %q/%d/%q",
					d.DeadlineID, d.DaysLeft, d.TenantID, deadlineID, ev.DaysLeft, ev.TenantID)
			}
		})
	}
}

// TestOnReminderCheck_Idempotent proves a replay (dedup reports seen) is a pure no-op: no
// re-read, no lembrete — the dedup floor guards the fire path as it guards creation.
func TestOnReminderCheck_Idempotent(t *testing.T) {
	ev := reminderCheckFixture(uuid.NewString(), uuid.NewString(), 1)
	repo := &mockRepo{checkResult: &DeadlineForCheck{ID: ev.DeadlineID, Status: StatusOpen}}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnReminderCheck(context.Background(), ev); err != nil {
		t.Fatalf("OnReminderCheck() error = %v", err)
	}
	if repo.checkCalls != 0 {
		t.Errorf("GetDeadlineForCheck calls = %d, want 0 on a replay", repo.checkCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
	if len(dedup.consumers) != 1 || dedup.consumers[0] != consumerDeadline {
		t.Errorf("dedup consumer = %v, want [%q]", dedup.consumers, consumerDeadline)
	}
}

// TestOnReminderCheck_DeadlineGoneIsTerminal proves a re-read that finds no prazo surfaces
// the typed not-found (KindNotFound → the listener SkipRetries it), emitting nothing.
func TestOnReminderCheck_DeadlineGoneIsTerminal(t *testing.T) {
	ev := reminderCheckFixture(uuid.NewString(), uuid.NewString(), 0)
	repo := &mockRepo{checkErr: ErrDeadlineNotFound}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	err := uc.OnReminderCheck(context.Background(), ev)
	aerr, ok := apperr.From(err)
	if !ok || aerr.Kind != apperr.KindNotFound {
		t.Errorf("error = %v, want KindNotFound", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on error", len(outbox.published))
	}
}

// TestOnMissedCheck_MarksMissed proves the auto-miss (decisão travada: MISSED auto D+1, SÓ
// em OPEN): when the guarded UPDATE flips a still-OPEN, overdue prazo it emits exactly one
// deadline.missed; when the guard touches no row (PENDING, terminal, or not yet overdue —
// all collapsed to ErrDeadlineNotFound) it is a pure no-op.
func TestOnMissedCheck_MarksMissed(t *testing.T) {
	tests := []struct {
		name     string
		markID   string
		markErr  error
		wantEmit bool
	}{
		{"OPEN & overdue → MISSED + event", "", nil, true},
		{"not OPEN / not overdue → no-op", "", ErrDeadlineNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deadlineID := uuid.NewString()
			ev := DeadlineMissedCheck{
				Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
				TenantID:   uuid.NewString(),
				DeadlineID: deadlineID,
			}
			repo := &mockRepo{markMissedErr: tt.markErr}
			if tt.markErr == nil {
				repo.markMissedID = deadlineID
			}
			outbox := &fakeOutbox{}
			uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

			if err := uc.OnMissedCheck(context.Background(), ev); err != nil {
				t.Fatalf("OnMissedCheck() error = %v", err)
			}

			// The flip was tenant-scoped to the event (barrier 1 args threaded).
			if repo.markMissedCalls != 1 || repo.gotMissedID != deadlineID || repo.gotMissedTenantID != ev.TenantID {
				t.Errorf("MarkMissed calls/id/tenant = %d/%q/%q, want 1/%q/%q",
					repo.markMissedCalls, repo.gotMissedID, repo.gotMissedTenantID, deadlineID, ev.TenantID)
			}

			missed := publishedOfType[DeadlineMissed](outbox)
			if !tt.wantEmit {
				if len(missed) != 0 {
					t.Fatalf("deadline.missed events = %d, want 0 (safe no-op)", len(missed))
				}
				return
			}
			if len(missed) != 1 {
				t.Fatalf("deadline.missed events = %d, want 1", len(missed))
			}
			m := missed[0]
			if m.Type() != TypeDeadlineMissed || m.AggregateType() != aggregateTypeDeadline {
				t.Errorf("missed type/aggregate = %q/%q", m.Type(), m.AggregateType())
			}
			if m.AggregateID() != deadlineID {
				t.Errorf("missed aggregate id = %q, want %q", m.AggregateID(), deadlineID)
			}
			if _, err := uuid.Parse(m.AggregateID()); err != nil {
				t.Errorf("missed aggregate id is not a uuid: %v", err)
			}
			if m.DeadlineID != deadlineID || m.TenantID != ev.TenantID {
				t.Errorf("missed deadline/tenant = %q/%q, want %q/%q", m.DeadlineID, m.TenantID, deadlineID, ev.TenantID)
			}
		})
	}
}

// TestOnMissedCheck_Idempotent proves a replay is a pure no-op: no flip, no event.
func TestOnMissedCheck_Idempotent(t *testing.T) {
	deadlineID := uuid.NewString()
	ev := DeadlineMissedCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   uuid.NewString(),
		DeadlineID: deadlineID,
	}
	repo := &mockRepo{markMissedID: deadlineID}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{seen: true}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, dedup, &fakeUOW{})

	if err := uc.OnMissedCheck(context.Background(), ev); err != nil {
		t.Fatalf("OnMissedCheck() error = %v", err)
	}
	if repo.markMissedCalls != 0 {
		t.Errorf("MarkMissed calls = %d, want 0 on a replay", repo.markMissedCalls)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on a replay", len(outbox.published))
	}
}

// TestOnMissedCheck_InfraErrorPropagates proves an infra fault from the flip aborts the tx
// (retryable) and emits nothing — only ErrDeadlineNotFound is a no-op.
func TestOnMissedCheck_InfraErrorPropagates(t *testing.T) {
	deadlineID := uuid.NewString()
	ev := DeadlineMissedCheck{
		Base:       events.Base{EventID: uuid.NewString(), Aggregate: deadlineID},
		TenantID:   uuid.NewString(),
		DeadlineID: deadlineID,
	}
	repo := &mockRepo{markMissedErr: apperr.NewInfra("db down", errors.New("boom"))}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, &fakeCalendar{}, outbox, &fakeDedup{}, &fakeUOW{})

	err := uc.OnMissedCheck(context.Background(), ev)
	aerr, ok := apperr.From(err)
	if !ok || aerr.Kind != apperr.KindInfra {
		t.Errorf("error = %v, want KindInfra", err)
	}
	if len(outbox.published) != 0 {
		t.Errorf("published events = %d, want 0 on error", len(outbox.published))
	}
}

// ── V1 Motor de Prazos: derive functions ──────────────────────────────────────

func TestDeriveOrigem(t *testing.T) {
	tests := []struct {
		name         string
		hasDeclarado bool
		usedIA       bool
		want         Origem
	}{
		{"declarado wins over IA", true, true, OrigemDeclarado},
		{"declarado without IA", true, false, OrigemDeclarado},
		{"IA only", false, true, OrigemIA},
		{"neither — calculado", false, false, OrigemCalculado},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveOrigem(tt.hasDeclarado, tt.usedIA)
			if got != tt.want {
				t.Errorf("deriveOrigem(%v, %v) = %q, want %q", tt.hasDeclarado, tt.usedIA, got, tt.want)
			}
		})
	}
}

func TestDeriveSeal(t *testing.T) {
	tests := []struct {
		name      string
		origem    Origem
		divergent bool
		want      Seal
	}{
		{"IA always a_apurar", OrigemIA, false, SealAApurar},
		{"divergent always a_apurar", OrigemCalculado, true, SealAApurar},
		{"IA + divergent", OrigemIA, true, SealAApurar},
		{"declarado normal", OrigemDeclarado, false, SealConfiavel},
		{"calculado normal", OrigemCalculado, false, SealConfiavel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveSeal(tt.origem, tt.divergent)
			if got != tt.want {
				t.Errorf("deriveSeal(%q, %v) = %q, want %q", tt.origem, tt.divergent, got, tt.want)
			}
		})
	}
}

func TestDeriveConfirmacaoExigida(t *testing.T) {
	tests := []struct {
		name              string
		seal              Seal
		policyObrigatoria bool
		want              bool
	}{
		{"seal a_apurar overrides policy", SealAApurar, false, true},
		{"seal a_apurar + policy true", SealAApurar, true, true},
		{"seal confiavel + policy obrigatoria", SealConfiavel, true, true},
		{"seal confiavel + policy seletiva", SealConfiavel, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveConfirmacaoExigida(tt.seal, tt.policyObrigatoria)
			if got != tt.want {
				t.Errorf("deriveConfirmacaoExigida(%q, %v) = %v, want %v", tt.seal, tt.policyObrigatoria, got, tt.want)
			}
		})
	}
}

// TestOnIntimationObserved_V1Fields verifies the V1 fields (origem, selo,
// confirmacao_exigida) are set on the born deadline and that the audit trail
// (calc_memory, deadline_event) is persisted in the same tx.
func TestOnIntimationObserved_V1Fields(t *testing.T) {
	tenantID := uuid.NewString()
	courtRecordID := uuid.NewString()
	intimationID := uuid.NewString()

	repo := &mockRepo{
		rule: DeadlineRule{
			RulesVersion:  rulesVersion,
			Kind:          "MANIFESTACAO",
			Days:          15,
			Counting:      CountingBusiness,
			LegalCitation: "art. 335, CPC",
		},
	}
	cal := &fakeCalendar{endDate: time.Now().AddDate(0, 0, 20)}
	outbox := &fakeOutbox{}
	dedup := &fakeDedup{}
	uow := &fakeUOW{}
	uc := NewUseCase(repo, cal, outbox, dedup, uow)

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   courtRecordID,
		IntimationID:    intimationID,
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2026-09-15",
	}

	err := uc.OnIntimationObserved(context.Background(), ev)
	if err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	// Verify deadline V1 fields
	d := repo.inserted
	if d == nil {
		t.Fatal("InsertDeadline not called")
	}
	if d.Origem != OrigemCalculado {
		t.Errorf("origem = %q, want %q (no declarado, no IA)", d.Origem, OrigemCalculado)
	}
	if d.Seal != SealConfiavel {
		t.Errorf("selo = %q, want %q (calculado + not divergent)", d.Seal, SealConfiavel)
	}
	if d.ConfirmacaoExigida {
		t.Error("confirmacao_exigida = true, want false (seal=confiavel + policy=seletiva)")
	}

	// Verify calc_memory was persisted
	if repo.gotCalcMemory == nil {
		t.Error("InsertCalcMemory not called")
	} else {
		if repo.gotCalcMemory.DeadlineID != d.ID {
			t.Errorf("calc_memory.deadline_id = %s, want %s", repo.gotCalcMemory.DeadlineID, d.ID)
		}
		if repo.gotCalcMemory.PrazoBase != "15 dias úteis" {
			t.Errorf("calc_memory.prazo_base = %q, want %q", repo.gotCalcMemory.PrazoBase, "15 dias úteis")
		}
		// The "por que essa data?" card must show human text, never the raw rule/anchor
		// identifiers (the bug this fix addresses).
		wantFonte := "art. 335, CPC · Tabela legal"
		if repo.gotCalcMemory.PrazoBaseFonte != wantFonte {
			t.Errorf("calc_memory.prazo_base_fonte = %q, want %q", repo.gotCalcMemory.PrazoBaseFonte, wantFonte)
		}
		if repo.gotCalcMemory.PrazoBaseFonte == "tabela_legal" {
			t.Error("calc_memory.prazo_base_fonte still the raw internal literal")
		}
		wantTermo := "Publicação no DJEN → contagem inicia no 1º dia útil seguinte (art. 224, §3º · 231, CPC)."
		if repo.gotCalcMemory.TermoInicialRegra != wantTermo {
			t.Errorf("calc_memory.termo_inicial_regra = %q, want %q", repo.gotCalcMemory.TermoInicialRegra, wantTermo)
		}
		if repo.gotCalcMemory.TermoInicialRegra == "deadline_start_at" {
			t.Error("calc_memory.termo_inicial_regra still the raw internal literal")
		}
	}

	// Verify deadline_event was persisted
	if repo.gotDeadlineEvent == nil {
		t.Error("InsertDeadlineEvent not called")
	} else {
		if repo.gotDeadlineEvent.DeadlineID != d.ID {
			t.Errorf("deadline_event.deadline_id = %s, want %s", repo.gotDeadlineEvent.DeadlineID, d.ID)
		}
		if repo.gotDeadlineEvent.Tipo != "calculado" {
			t.Errorf("deadline_event.tipo = %q, want %q", repo.gotDeadlineEvent.Tipo, "calculado")
		}
	}

	// V1 events: opened + calculated + seal_assigned + 3 reminders + 1 missed = 7
	if got := len(outbox.published); got != 7 {
		names := make([]string, got)
		for i, e := range outbox.published {
			names[i] = e.Type()
		}
		t.Errorf("published events = %d (%v), want 7 (opened + calculated + seal_assigned + 3 reminders + missed)", got, names)
	}

	// No skipped days were returned by AddBusinessDays (fakeCalendar.holidays unset), so the
	// name/scope lookup must not run at all — it is an audit-trail label pass, never on the
	// path that decides the date.
	if cal.lookupCalls != 0 {
		t.Errorf("LookupHolidays calls = %d, want 0 (no holidays skipped)", cal.lookupCalls)
	}
}

// TestOnIntimationObserved_AppliedHolidayLabels verifies the "por que essa data?" card gets a
// human holiday name/âmbito resolved from the calendar's LookupHolidays, not the hardcoded
// "feriado"/"nacional" this fix replaces — and that an unresolved date (drift between
// AddBusinessDays' skip list and LookupHolidays' rows) still degrades to a defensive label
// instead of leaving the fields empty.
func TestOnIntimationObserved_AppliedHolidayLabels(t *testing.T) {
	tenantID := uuid.NewString()
	feriadoEstadual := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	feriadoSemLabel := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)

	repo := &mockRepo{
		rule: DeadlineRule{
			RulesVersion: rulesVersion,
			Kind:         "MANIFESTACAO",
			Days:         15,
			Counting:     CountingBusiness,
		},
	}
	cal := &fakeCalendar{
		endDate:  time.Now().AddDate(0, 0, 20),
		holidays: []time.Time{feriadoEstadual, feriadoSemLabel},
		lookupLabel: map[time.Time]calendar.Holiday{
			feriadoEstadual: {Scope: calendar.ScopeState, ScopeID: "SP", Name: "Revolução Constitucionalista"},
			// feriadoSemLabel deliberately absent: exercises the drift fallback.
		},
	}
	uc := NewUseCase(repo, cal, &fakeOutbox{}, &fakeDedup{}, &fakeUOW{})

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2026-09-15",
	}

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	if cal.lookupCalls != 1 {
		t.Fatalf("LookupHolidays calls = %d, want 1", cal.lookupCalls)
	}
	if cal.lookupUF != "SP" || cal.lookupCourt != "TJSP" {
		t.Errorf("LookupHolidays(uf=%q, court=%q), want (SP, TJSP)", cal.lookupUF, cal.lookupCourt)
	}

	if len(repo.gotAppliedHolidays) != 2 {
		t.Fatalf("InsertAppliedHoliday calls = %d, want 2", len(repo.gotAppliedHolidays))
	}
	byDate := make(map[time.Time]*AppliedHoliday, len(repo.gotAppliedHolidays))
	for _, h := range repo.gotAppliedHolidays {
		byDate[h.Data] = h
	}

	resolved, ok := byDate[feriadoEstadual]
	if !ok {
		t.Fatal("applied_holiday missing the resolved-label date")
	}
	if resolved.Nome != "Revolução Constitucionalista" {
		t.Errorf("applied_holiday.nome = %q, want %q", resolved.Nome, "Revolução Constitucionalista")
	}
	if resolved.Ambito != "estadual" {
		t.Errorf("applied_holiday.ambito = %q, want %q", resolved.Ambito, "estadual")
	}

	fallback, ok := byDate[feriadoSemLabel]
	if !ok {
		t.Fatal("applied_holiday missing the unresolved-label date")
	}
	if fallback.Nome != "Feriado ou suspensão" {
		t.Errorf("applied_holiday.nome = %q, want the defensive fallback %q", fallback.Nome, "Feriado ou suspensão")
	}
	if fallback.Ambito != "nacional" {
		t.Errorf("applied_holiday.ambito = %q, want the defensive fallback %q", fallback.Ambito, "nacional")
	}

	// Neither field is the old hardcoded literal for both rows — the bug this fix addresses.
	for _, h := range repo.gotAppliedHolidays {
		if h.Nome == "feriado" {
			t.Errorf("applied_holiday.nome = %q, still the old hardcoded literal", h.Nome)
		}
	}
}

// TestOnIntimationObserved_ConfirmacaoExigida_PolicyObrigatoria verifies the
// piso inegociável: even with seal=confiavel, a tenant with ConfirmacaoObrigatoria=true
// gets confirmacao_exigida=true and a deadline.confirmation_required event.
func TestOnIntimationObserved_ConfirmacaoExigida_PolicyObrigatoria(t *testing.T) {
	tenantID := uuid.NewString()

	repo := &mockRepo{
		rule: DeadlineRule{
			RulesVersion: rulesVersion,
			Kind:         "MANIFESTACAO",
			Days:         15,
			Counting:     CountingBusiness,
		},
		policy: DeadlinePolicy{
			TenantID:               tenantID,
			ConfirmacaoObrigatoria: true,
		},
	}
	cal := &fakeCalendar{endDate: time.Now().AddDate(0, 0, 20)}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2026-09-15",
	}

	err := uc.OnIntimationObserved(context.Background(), ev)
	if err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	d := repo.inserted
	if d == nil {
		t.Fatal("InsertDeadline not called")
	}
	if !d.ConfirmacaoExigida {
		t.Error("confirmacao_exigida = false, want true (policy obrigatoria)")
	}
	if d.Seal != SealConfiavel {
		t.Errorf("selo = %q, want %q", d.Seal, SealConfiavel)
	}

	// V1 events: opened + calculated + seal_assigned + confirmation_required + 3 reminders + 1 missed = 8
	if got := len(outbox.published); got != 8 {
		names := make([]string, got)
		for i, e := range outbox.published {
			names[i] = e.Type()
		}
		t.Errorf("published events = %d (%v), want 8 (opened + calculated + seal_assigned + confirmation_required + 3 reminders + missed)", got, names)
	}
}

// TestOnIntimationObserved_DeclaradoOrigem verifies that when PrazoDeclarado is present,
// the origem is "declarado" and seal is "confiavel".
func TestOnIntimationObserved_DeclaradoOrigem(t *testing.T) {
	tenantID := uuid.NewString()

	repo := &mockRepo{
		rule: DeadlineRule{
			RulesVersion: rulesVersion,
			Kind:         "MANIFESTACAO",
			Days:         15,
			Counting:     CountingBusiness,
		},
	}
	cal := &fakeCalendar{endDate: time.Now().AddDate(0, 0, 20)}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2026-09-15",
		PrazoDeclarado:  "5 dias",
	}

	err := uc.OnIntimationObserved(context.Background(), ev)
	if err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	d := repo.inserted
	if d == nil {
		t.Fatal("InsertDeadline not called")
	}
	if d.Origem != OrigemDeclarado {
		t.Errorf("origem = %q, want %q", d.Origem, OrigemDeclarado)
	}
	if d.Seal != SealConfiavel {
		t.Errorf("selo = %q, want %q (declarado + not divergent = confiavel)", d.Seal, SealConfiavel)
	}
}

// --- parseDeclaradoDays / buildCrossValidation (pure, unit) ------------------

func TestParseDeclaradoDays(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		in     string
		wantN  int
		wantOK bool
	}{
		{name: "simple dias", in: "5 dias", wantN: 5, wantOK: true},
		{name: "dias uteis suffix", in: "15 dias úteis", wantN: 15, wantOK: true},
		{name: "leading whitespace", in: "  10 dias", wantN: 10, wantOK: true},
		{name: "empty", in: "", wantN: 0, wantOK: false},
		{name: "non numeric", in: "prazo indeterminado", wantN: 0, wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n, ok := parseDeclaradoDays(tt.in)
			if n != tt.wantN || ok != tt.wantOK {
				t.Errorf("parseDeclaradoDays(%q) = (%d, %v), want (%d, %v)", tt.in, n, ok, tt.wantN, tt.wantOK)
			}
		})
	}
}

func TestBuildCrossValidation(t *testing.T) {
	t.Parallel()

	declared := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	t.Run("convergente when dates match", func(t *testing.T) {
		t.Parallel()
		cv := buildCrossValidation("tenant-1", declared, declared, nil)
		if cv.Resultado != crossValidationConvergente {
			t.Errorf("resultado = %q, want %q", cv.Resultado, crossValidationConvergente)
		}
		if cv.DifDias != 0 {
			t.Errorf("dif_dias = %d, want 0", cv.DifDias)
		}
		if cv.CausaProvavel != "" {
			t.Errorf("causa_provavel = %q, want empty on convergente", cv.CausaProvavel)
		}
	})

	t.Run("divergente when dates differ, dif_dias signed", func(t *testing.T) {
		t.Parallel()
		calculated := declared.AddDate(0, 0, 10)
		cv := buildCrossValidation("tenant-1", declared, calculated, nil)
		if cv.Resultado != crossValidationDivergente {
			t.Errorf("resultado = %q, want %q", cv.Resultado, crossValidationDivergente)
		}
		if cv.DifDias != 10 {
			t.Errorf("dif_dias = %d, want 10", cv.DifDias)
		}
		if cv.CausaProvavel == "" {
			t.Error("causa_provavel = empty, want a heuristic explanation on divergente")
		}
	})

	t.Run("divergente causa mentions applied holidays when present", func(t *testing.T) {
		t.Parallel()
		calculated := declared.AddDate(0, 0, 3)
		holidays := []time.Time{declared.AddDate(0, 0, 1)}
		cv := buildCrossValidation("tenant-1", declared, calculated, holidays)
		if cv.CausaProvavel == "" {
			t.Fatal("causa_provavel = empty, want holiday-aware heuristic")
		}
	})
}

// --- OnIntimationObserved: cross-validation + IA classification (integration) ---

// TestOnIntimationObserved_CrossValidation_Divergente verifies that when the declared and
// calculated day counts differ, InsertCrossValidation is called with resultado=divergente and
// the deadline's selo is forced to a_apurar (the piso inegociável), even though origem stays
// "declarado" (the intimação DID declare a prazo).
func TestOnIntimationObserved_CrossValidation_Divergente(t *testing.T) {
	tenantID := uuid.NewString()

	repo := &mockRepo{
		rule:     DeadlineRule{RulesVersion: rulesVersion, Kind: "MANIFESTACAO", Days: 15, Counting: CountingBusiness},
		insertID: uuid.NewString(),
	}
	// endDate left zero so fakeCalendar computes start.AddDate(0,0,n) per call — the declared
	// (5 dias) and calculado (15 dias) calls then land on genuinely different dates.
	cal := &fakeCalendar{}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2099-01-01",
		PrazoDeclarado:  "5 dias",
	}

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	if repo.gotCrossValidation == nil {
		t.Fatal("InsertCrossValidation not called")
	}
	if repo.gotCrossValidation.Resultado != crossValidationDivergente {
		t.Errorf("cross_validation.resultado = %q, want %q", repo.gotCrossValidation.Resultado, crossValidationDivergente)
	}
	if repo.gotCrossValidation.DifDias != 10 {
		t.Errorf("cross_validation.dif_dias = %d, want 10", repo.gotCrossValidation.DifDias)
	}
	if repo.gotCrossValidation.DeadlineID != repo.insertID {
		t.Errorf("cross_validation.deadline_id = %q, want %q (the saved deadline's id)", repo.gotCrossValidation.DeadlineID, repo.insertID)
	}

	d := repo.inserted
	if d.Origem != OrigemDeclarado {
		t.Errorf("origem = %q, want %q (a divergência does not change origem)", d.Origem, OrigemDeclarado)
	}
	if d.Seal != SealAApurar {
		t.Errorf("selo = %q, want %q (divergent = piso inegociável)", d.Seal, SealAApurar)
	}
	if !d.ConfirmacaoExigida {
		t.Error("confirmacao_exigida = false, want true (piso inegociável on a_apurar)")
	}
}

// TestOnIntimationObserved_CrossValidation_Convergente verifies that when the declared and
// calculated day counts match, InsertCrossValidation is called with resultado=convergente and
// selo stays confiavel.
func TestOnIntimationObserved_CrossValidation_Convergente(t *testing.T) {
	tenantID := uuid.NewString()

	repo := &mockRepo{
		rule:     DeadlineRule{RulesVersion: rulesVersion, Kind: "MANIFESTACAO", Days: 15, Counting: CountingBusiness},
		insertID: uuid.NewString(),
	}
	cal := &fakeCalendar{}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2099-01-01",
		PrazoDeclarado:  "15 dias",
	}

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	if repo.gotCrossValidation == nil {
		t.Fatal("InsertCrossValidation not called")
	}
	if repo.gotCrossValidation.Resultado != crossValidationConvergente {
		t.Errorf("cross_validation.resultado = %q, want %q", repo.gotCrossValidation.Resultado, crossValidationConvergente)
	}
	if repo.inserted.Seal != SealConfiavel {
		t.Errorf("selo = %q, want %q (convergent + declarado = confiavel)", repo.inserted.Seal, SealConfiavel)
	}
}

// TestOnIntimationObserved_CrossValidation_UnparseableDeclaracao verifies that a
// prazo_declarado the extractor cannot resolve to a day count (free text with no leading
// integer) skips cross-validation entirely — no InsertCrossValidation call, no error — while
// origem still derives "declarado" (the intimação DID declare something).
func TestOnIntimationObserved_CrossValidation_UnparseableDeclaracao(t *testing.T) {
	tenantID := uuid.NewString()

	repo := &mockRepo{
		rule: DeadlineRule{RulesVersion: rulesVersion, Kind: "MANIFESTACAO", Days: 15, Counting: CountingBusiness},
	}
	cal := &fakeCalendar{endDate: time.Now().AddDate(0, 0, 20)}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{})

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2026-09-15",
		PrazoDeclarado:  "prazo indeterminado",
	}

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	if repo.gotCrossValidation != nil {
		t.Errorf("InsertCrossValidation called = %+v, want no call (unparseable declaração)", repo.gotCrossValidation)
	}
	if repo.inserted.Origem != OrigemDeclarado {
		t.Errorf("origem = %q, want %q", repo.inserted.Origem, OrigemDeclarado)
	}
}

// fakeClassifier is a typeClassifier double: returns a configured ClassifiedType/error and
// records the CaseContext it was called with.
type fakeClassifier struct {
	result ClassifiedType
	err    error
	calls  int
	gotCtx advisory.CaseContext
}

func (f *fakeClassifier) ClassifyType(_ context.Context, _ string, c advisory.CaseContext) (ClassifiedType, error) {
	f.calls++
	f.gotCtx = c
	return f.result, f.err
}

// TestOnIntimationObserved_OmissaClassifiedByIA verifies that when the intimação is omissa (no
// prazo_declarado) and a classifier is wired + succeeds, origem derives "ia" (usedIA=true) and
// selo is forced to a_apurar (the piso inegociável) — and the classified tipo/confiança land in
// calc_memory (provenance).
func TestOnIntimationObserved_OmissaClassifiedByIA(t *testing.T) {
	tenantID := uuid.NewString()

	repo := &mockRepo{
		rule: DeadlineRule{RulesVersion: rulesVersion, Kind: "GENERICO", Days: 15, Counting: CountingBusiness},
	}
	cal := &fakeCalendar{endDate: time.Now().AddDate(0, 0, 20)}
	outbox := &fakeOutbox{}
	classifier := &fakeClassifier{result: ClassifiedType{Tipo: "Contestação", Confianca: 0.8}}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{}, WithClassifier(classifier))

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2026-09-15",
		// PrazoDeclarado deliberately empty — omissa intimação.
	}

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	if classifier.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1", classifier.calls)
	}
	d := repo.inserted
	if d.Origem != OrigemIA {
		t.Errorf("origem = %q, want %q", d.Origem, OrigemIA)
	}
	if d.Seal != SealAApurar {
		t.Errorf("selo = %q, want %q (origem=ia is a piso inegociável)", d.Seal, SealAApurar)
	}
	if repo.gotCalcMemory == nil || repo.gotCalcMemory.IATipoInferido != "Contestação" {
		t.Errorf("calc_memory.ia_tipo_inferido = %+v, want %q", repo.gotCalcMemory, "Contestação")
	}
	if repo.gotCalcMemory.IAConfianca != 0.8 {
		t.Errorf("calc_memory.ia_confianca = %v, want 0.8", repo.gotCalcMemory.IAConfianca)
	}
}

// TestOnIntimationObserved_OmissaClassifierUnavailable verifies that an omissa intimação with
// NO classifier wired (nil, the common worker/api composition when OpenRouter is unconfigured)
// never fails the ingest — the prazo still opens, just without the IA enrichment (origem stays
// "calculado", never chuta a tipo).
func TestOnIntimationObserved_OmissaClassifierUnavailable(t *testing.T) {
	tenantID := uuid.NewString()

	repo := &mockRepo{
		rule: DeadlineRule{RulesVersion: rulesVersion, Kind: "GENERICO", Days: 15, Counting: CountingBusiness},
	}
	cal := &fakeCalendar{endDate: time.Now().AddDate(0, 0, 20)}
	outbox := &fakeOutbox{}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{}) // no WithClassifier

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2026-09-15",
	}

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v", err)
	}

	d := repo.inserted
	if d == nil {
		t.Fatal("InsertDeadline not called")
	}
	if d.Origem != OrigemCalculado {
		t.Errorf("origem = %q, want %q (no classifier wired, never chuta)", d.Origem, OrigemCalculado)
	}
}

// TestOnIntimationObserved_OmissaClassifierFailureDoesNotBlockIngest verifies that a
// classifier error (LLM call failed, timed out, etc — anything other than
// ErrClassifierUnavailable) is swallowed (logged, not propagated): the design's floor is
// "prazo sempre nasce", so an OPTIONAL enrichment failure never fails the ingest.
func TestOnIntimationObserved_OmissaClassifierFailureDoesNotBlockIngest(t *testing.T) {
	tenantID := uuid.NewString()

	repo := &mockRepo{
		rule: DeadlineRule{RulesVersion: rulesVersion, Kind: "GENERICO", Days: 15, Counting: CountingBusiness},
	}
	cal := &fakeCalendar{endDate: time.Now().AddDate(0, 0, 20)}
	outbox := &fakeOutbox{}
	classifier := &fakeClassifier{err: errors.New("llm: timeout")}
	uc := NewUseCase(repo, cal, outbox, &fakeDedup{}, &fakeUOW{}, WithClassifier(classifier))

	ev := IntimationObserved{
		Base:            events.Base{EventID: uuid.NewString()},
		TenantID:        tenantID,
		CourtRecordID:   uuid.NewString(),
		IntimationID:    uuid.NewString(),
		Type:            "INTIMACAO",
		Court:           "TJSP",
		UF:              "SP",
		DeadlineStartAt: "2026-09-15",
	}

	if err := uc.OnIntimationObserved(context.Background(), ev); err != nil {
		t.Fatalf("OnIntimationObserved() error = %v, want nil (classifier failure must not block ingest)", err)
	}
	if repo.inserted.Origem != OrigemCalculado {
		t.Errorf("origem = %q, want %q (classifier failed, never chuta)", repo.inserted.Origem, OrigemCalculado)
	}
}
