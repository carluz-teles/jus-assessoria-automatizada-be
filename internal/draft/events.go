package draft

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/lib/events"
)

// events.go defines the events the draft slice PRODUCES for Fatia 3. The generation trigger
// (draft.generation_requested) is published on the write path (POST /v1/pecas/:id/generate)
// and consumed by worker-ai. The review completion fact (review.completed) is published by
// ReviewUseCase.ReviewDraft (review.go, the SYNCHRONOUS Revisar use case) inside the same tx
// that persists the review row, and is routed to the "notifications" queue (see relay routing
// in lib/events/relay.go) — acquisition's activity listener consumes it for the process
// cockpit's "Atividade" timeline (migration 0073, event DRAFT_GENERATED).

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

// ReviewCompleted is published by ReviewUseCase.ReviewDraft (review.go) inside the
// SAME tx that persists the review row. It announces a finished (COMPLETED) review —
// the advogado's synchronous "Revisar" call produced AI suggestions over the minuta.
// Consumers: acquisition's activity listener (process cockpit "Atividade" timeline,
// migration 0073 — DRAFT_GENERATED). TenantID is REQUIRED so a consumer can scope its
// RLS-guarded tx without a second, tenant-less lookup (docs §4d.4 barrier 1).
type ReviewCompleted struct {
	events.Base
	DraftID  string `json:"draft_id"`
	ReviewID string `json:"review_id"`
	TenantID string `json:"tenant_id"`
	Status   string `json:"status"` // COMPLETED | FAILED
}

var _ events.Event = ReviewCompleted{}

func (ReviewCompleted) Type() string          { return TypeReviewCompleted }
func (ReviewCompleted) AggregateType() string { return aggregateTypeDraft }

// newReviewCompleted builds the event after the review row is persisted. aggregate_id =
// draft_id (uuid); event_id is a fresh uuid v7 (consumer dedup key).
func newReviewCompleted(draftID, reviewID, tenantID, status string) ReviewCompleted {
	return ReviewCompleted{
		Base:     events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: draftID},
		DraftID:  draftID,
		ReviewID: reviewID,
		TenantID: tenantID,
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

// ── Fatia 1 — peticionamento automático (e-SAJ RPA) ───────────────────────────

// TypeFilingEnqueued is published inside the approve tx (POST /v1/pecas/:id/filing/approve)
// right after the filing_attempt is inserted as ENFILEIRADO. worker-filing consumes it
// on the "filing" queue (concurrency 1) and runs the e-SAJ RPA.
const TypeFilingEnqueued = "filing.enqueued"

// FilingEnqueued carries the ids the worker needs: the frozen PDF (via
// filing_attempt, acessado por FilingAttemptID) e o draft. A credencial e-SAJ
// NÃO viaja no evento — o worker re-busca a ativa por (tenant_id, owner) dentro
// da tx, evitando stale credential e respeitando o isolamento de tenant. EventID
// é determinístico por attempt ("filing:<draftID>:<attemptID>") pra que um
// double-enqueue da MESMA tentativa seja dedupado na camada asynq, enquanto uma
// re-tentativa após FALHOU ganha novo attempt id → novo EventID → reprocessado
// (critério 4). Também é a chave de dedup do consumer em processed_event.
type FilingEnqueued struct {
	events.Base
	DraftID         string `json:"draft_id"`
	TenantID        string `json:"tenant_id"`
	FilingAttemptID string `json:"filing_attempt_id"`
}

var _ events.Event = FilingEnqueued{}

func (FilingEnqueued) Type() string          { return TypeFilingEnqueued }
func (FilingEnqueued) AggregateType() string { return aggregateTypeDraft }

func newFilingEnqueued(draftID, tenantID, attemptID string) FilingEnqueued {
	return FilingEnqueued{
		Base: events.Base{
			EventID:   "filing:" + draftID + ":" + attemptID,
			Aggregate: draftID,
		},
		DraftID:         draftID,
		TenantID:        tenantID,
		FilingAttemptID: attemptID,
	}
}

// TypeFilingSucceeded is published by worker-filing after a successful e-SAJ protocol.
// notifications consumes it → in-app aviso via SSE.
const TypeFilingSucceeded = "filing.succeeded"

// FilingSucceeded announces a successful automated protocol, carrying the tribunal's
// protocol number (filing_number) and the created petition id.
type FilingSucceeded struct {
	events.Base
	DraftID         string `json:"draft_id"`
	TenantID        string `json:"tenant_id"`
	FilingAttemptID string `json:"filing_attempt_id"`
	PetitionID      string `json:"petition_id"`
	FilingNumber    string `json:"filing_number"`
}

var _ events.Event = FilingSucceeded{}

func (FilingSucceeded) Type() string          { return TypeFilingSucceeded }
func (FilingSucceeded) AggregateType() string { return aggregateTypeDraft }

func newFilingSucceeded(draftID, tenantID, attemptID, petitionID, filingNumber string) FilingSucceeded {
	return FilingSucceeded{
		Base: events.Base{
			EventID:   uuid.Must(uuid.NewV7()).String(),
			Aggregate: draftID,
		},
		DraftID:         draftID,
		TenantID:        tenantID,
		FilingAttemptID: attemptID,
		PetitionID:      petitionID,
		FilingNumber:    filingNumber,
	}
}

// TypeFilingFailed is published by worker-filing when the e-SAJ RPA fails. The
// filing_attempt is marked FALHOU with the reason; the manual fallback remains available.
const TypeFilingFailed = "filing.failed"

// FilingFailed announces a failed automated protocol so the user can retry or fall back
// to the manual "Marcar como protocolada" flow.
type FilingFailed struct {
	events.Base
	DraftID         string `json:"draft_id"`
	TenantID        string `json:"tenant_id"`
	FilingAttemptID string `json:"filing_attempt_id"`
	FailureReason   string `json:"failure_reason"`
}

var _ events.Event = FilingFailed{}

func (FilingFailed) Type() string          { return TypeFilingFailed }
func (FilingFailed) AggregateType() string { return aggregateTypeDraft }

func newFilingFailed(draftID, tenantID, attemptID, reason string) FilingFailed {
	return FilingFailed{
		Base: events.Base{
			EventID:   uuid.Must(uuid.NewV7()).String(),
			Aggregate: draftID,
		},
		DraftID:         draftID,
		TenantID:        tenantID,
		FilingAttemptID: attemptID,
		FailureReason:   reason,
	}
}
