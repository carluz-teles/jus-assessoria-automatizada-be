package draft

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

// events.go defines the events the draft slice PRODUCES for Fatia 3. The generation trigger
// (draft.generation_requested) is published on the write path (POST /v1/pecas/:id/generate)
// and consumed by worker-ai. The review completion fact (review.completed) is published by
// the worker-ai listener after a successful generation and is routed to an archivable queue
// (no active consumer today — see relay routing in lib/events/relay.go).

const aggregateTypeDraft = "draft"

// TypeGenerationRequested is the dotted id the relay routes to the "ai" queue and
// worker-ai consumes. Only the const crosses the slice boundary; the payload is
// decoded locally by the worker's listener.
const TypeGenerationRequested = "draft.generation_requested"

// TypeReviewCompleted is the dotted id published after a successful generation. Its
// "draft" prefix sends it to "default" via the relay's prefix switch. No worker
// drains "default" today, but routing to "default" (instead of "ai") prevents buildup
// in a loaded queue. String literal matches the relay routing comment.
const TypeReviewCompleted = "review.completed"

// GenerationRequested is published by POST /v1/pecas/:id/generate inside the same tx
// that flips saga_state to EXTRACTING. The consumer (worker-ai) reads it to kick off
// the LLM generation pipeline. TenantID scopes the consumer's RLS-guarded tx.
type GenerationRequested struct {
	events.Base
	DraftID  string `json:"draft_id"`
	TenantID string `json:"tenant_id"`
}

var _ events.Event = GenerationRequested{}

func (GenerationRequested) Type() string          { return TypeGenerationRequested }
func (GenerationRequested) AggregateType() string { return aggregateTypeDraft }

// newGenerationRequested builds the event from a persisted draft. aggregate_id = draft_id
// (uuid, satisfies outbox NOT NULL); event_id is a fresh uuid v7 (consumer dedup key).
func newGenerationRequested(d *Draft) GenerationRequested {
	return GenerationRequested{
		Base:     events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: d.ID},
		DraftID:  d.ID,
		TenantID: d.TenantID,
	}
}

// ReviewCompleted is published by the worker-ai listener inside the same tx that
// persists the review row. It announces a finished (COMPLETED or FAILED) generation.
// Downstream consumers (future: notifications, read-model invalidation) subscribe here.
type ReviewCompleted struct {
	events.Base
	DraftID  string `json:"draft_id"`
	ReviewID string `json:"review_id"`
	Status   string `json:"status"` // COMPLETED | FAILED
}

var _ events.Event = ReviewCompleted{}

func (ReviewCompleted) Type() string          { return TypeReviewCompleted }
func (ReviewCompleted) AggregateType() string { return aggregateTypeDraft }

// newReviewCompleted builds the event after the review row is persisted. aggregate_id =
// draft_id (uuid); event_id is a fresh uuid v7 (consumer dedup key).
func newReviewCompleted(draftID, reviewID, status string) ReviewCompleted {
	return ReviewCompleted{
		Base:     events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: draftID},
		DraftID:  draftID,
		ReviewID: reviewID,
		Status:   status,
	}
}

// ── Fatia 4 — peticionamento events ─────────────────────────────────────────

// TypeDraftSigned is published after the draft status transitions to SIGNED.
const TypeDraftSigned = "draft.signed"

// DraftSigned announces that a peça has been signed. Downstream consumers
// (notifications, read-model invalidation) subscribe here.
type DraftSigned struct {
	events.Base
	DraftID  string `json:"draft_id"`
	TenantID string `json:"tenant_id"`
}

var _ events.Event = DraftSigned{}

func (DraftSigned) Type() string          { return TypeDraftSigned }
func (DraftSigned) AggregateType() string { return aggregateTypeDraft }

func newDraftSigned(d *Draft) DraftSigned {
	return DraftSigned{
		Base:     events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: d.ID},
		DraftID:  d.ID,
		TenantID: d.TenantID,
	}
}

// TypePetitionFiled is published after a petition is created (peça filed).
const TypePetitionFiled = "petition.filed"

// PetitionFiled announces that a signed peça has been filed. The consumer
// (notifications, read-model) uses it to update the draft's saga_state display.
type PetitionFiled struct {
	events.Base
	PetitionID string `json:"petition_id"`
	DraftID    string `json:"draft_id"`
	TenantID   string `json:"tenant_id"`
}

var _ events.Event = PetitionFiled{}

func (PetitionFiled) Type() string          { return TypePetitionFiled }
func (PetitionFiled) AggregateType() string { return aggregateTypeDraft }

func newPetitionFiled(p *Petition, tenantID string) PetitionFiled {
	return PetitionFiled{
		Base:       events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: p.DraftID},
		PetitionID: p.ID,
		DraftID:    p.DraftID,
		TenantID:   tenantID,
	}
}
