package draft

import (
	"time"

	"github.com/jusassessoria/platform/internal/draft/draftdb"
)

// DraftDetailView is the read model for GET /v1/pecas/:id. It is a dedicated
// projection from the JOIN query — not the aggregate mounted in Go — so the handler
// never builds the aggregated entity for a read. All nested structs are nil when the
// draft has no intimation.
type DraftDetailView struct {
	ID        string
	PieceType string
	Title     string
	Content   string
	Status    string
	SagaState string
	CreatedAt time.Time
	UpdatedAt time.Time

	// Intimation is nil for blank/processo drafts.
	Intimation *IntimationView
	// Process is nil when no intimation (and therefore no court_record) is linked.
	Process *ProcessView
	// Deadline is nil when no deadline has been derived for the intimation yet.
	Deadline *DeadlineView
	// Attachments is the ordered list of uploaded documents linked to this draft.
	// Empty slice (never nil) when no attachments exist.
	Attachments []Attachment
}

// IntimationView is the context the editor shows alongside the draft text.
type IntimationView struct {
	ID              string
	Type            string
	Content         string
	MadeAvailableAt time.Time
	DeadlineStartAt time.Time
}

// ProcessView is the processo context — from court_record — shown in the editor
// sidebar.
type ProcessView struct {
	CaseID        string
	CourtRecordID string
	CNJNumber     string
	Court         string
	Degree        string
	Class         string
	Subject       string
	JudgingBody   string
}

// DeadlineView is the prazo context shown in the editor header bar. DaysLeft is
// computed at query time from end_date; the handler derives it for the response.
type DeadlineView struct {
	ID      string
	EndDate time.Time
	Status  string
}

// detailViewFromRow maps the GetDraftDetail row to the read model. All nested
// structs are populated only when their anchor id is non-NULL (the LEFT JOIN may
// yield NULLs for a blank/processo draft).
func detailViewFromRow(r draftdb.GetDraftDetailRow) *DraftDetailView {
	view := &DraftDetailView{
		ID:          r.ID.String(),
		PieceType:   r.PieceType,
		Title:       r.Title,
		Content:     derefString(r.Content),
		Status:      r.Status,
		SagaState:   r.SagaState,
		CreatedAt:   timestamptzToTime(r.CreatedAt),
		UpdatedAt:   timestamptzToTime(r.UpdatedAt),
		Attachments: []Attachment{}, // never nil — serializes as [] not null
	}

	// Intimation — only when the draft has an intimation_id (LEFT JOIN yields a
	// valid UUID when present).
	if r.IntimationID.Valid {
		view.Intimation = &IntimationView{
			ID:              pgUUIDToString(r.IntimationID),
			Type:            derefString(r.IntimationType),
			Content:         derefString(r.IntimationContent),
			MadeAvailableAt: pgDateToTime(r.IntimationMadeAvailableAt),
			DeadlineStartAt: pgDateToTime(r.IntimationDeadlineStartAt),
		}
	}

	// Process — only when a court_record was joined (requires intimation).
	if r.ProcessCourtRecordID.Valid {
		view.Process = &ProcessView{
			CaseID:        pgUUIDToString(r.ProcessCaseID),
			CourtRecordID: pgUUIDToString(r.ProcessCourtRecordID),
			CNJNumber:     derefString(r.ProcessCnjNumber),
			Court:         derefString(r.ProcessCourt),
			Degree:        derefString(r.ProcessDegree),
			Class:         derefString(r.ProcessClass),
			Subject:       derefString(r.ProcessSubject),
			JudgingBody:   derefString(r.ProcessJudgingBody),
		}
	}

	// Deadline — only when one has been derived for this intimation.
	if r.DeadlineID.Valid {
		view.Deadline = &DeadlineView{
			ID:      pgUUIDToString(r.DeadlineID),
			EndDate: pgDateToTime(r.DeadlineEndDate),
			Status:  derefString(r.DeadlineStatus),
		}
	}

	return view
}
