package actionitem

import (
	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/events"
)

// TypeIntimationAnalyzed is the dotted id this slice CONSUMES. Only the const crosses the
// boundary from acquisition (the producer); the payload SHAPE is redefined LOCALLY as
// IntimationAnalyzed below, so this slice never imports acquisition's event struct. A
// contract round-trip test (events_test.go) marshals the producer's struct and unmarshals
// it here, guarding against silent field drift (memória
// parallel-producer-consumer-roundtrip).
const TypeIntimationAnalyzed = acquisition.TypeIntimationAnalyzed

// ProvidenciaCandidate is the LOCAL decode shape of one candidate inside
// acquisition.IntimationAnalyzed's Providencias array — the exact subset this slice reads
// to materialize an action_item row (docs §3's precedence machine). PieceProfileKey is a
// pointer so an absent suggestion (gera_peca=false) round-trips as JSON null, not "".
type ProvidenciaCandidate struct {
	Tipo            string   `json:"tipo"`
	GeraPeca        bool     `json:"gera_peca"`
	PieceProfileKey *string  `json:"piece_profile_key"`
	Declarado       bool     `json:"declarado"`
	Confianca       *float64 `json:"confianca"`
}

// IntimationAnalyzed is the LOCAL decode shape of acquisition.intimation.analyzed: the
// exact subset of fields this slice's listener reads. This struct is only ever DECODED
// here (events.Decode needs no interface), so it carries no Type()/AggregateType(). Base
// yields the event id for dedup.
type IntimationAnalyzed struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	IntimationID  string `json:"intimation_id"`
	CourtRecordID string `json:"court_record_id"`
	// DeadlineID is the prazo already derived for this intimação at analysis time ("" when
	// none exists yet) — copied onto every materialized action_item's deadline_id.
	DeadlineID   string                 `json:"deadline_id"`
	Providencias []ProvidenciaCandidate `json:"providencias"`
}

const aggregateTypeActionItem = "action_item"

// TypeActionItemCreated is emitted when a materialized providência is born already
// confiável (declarado/manual — no human confirmation needed): the future deadline
// listener reacts to it by creating the task right away (docs §3, "nasce pronto").
const TypeActionItemCreated = "actionitem.created"

// TypeActionItemConfirmed is emitted by Confirmar: an a_confirmar item's tipo just turned
// confiável by the lawyer's hand. Same downstream effect as ActionItemCreated (task
// creation), just deferred to the human gesture instead of automatic at materialization.
const TypeActionItemConfirmed = "actionitem.confirmed"

// TypeActionItemDiscarded is emitted by Descartar: the providência is dismissed, status
// DISCARDED. No downstream task creation follows it.
const TypeActionItemDiscarded = "actionitem.discarded"

// actionItemEventPayload is the shared shape of all three events below — the minimal set a
// downstream consumer (the future deadline listener) needs to decide whether/how to act:
// which providência, which tenant/intimação, its tipo + peça classification, and the prazo
// it is regida by (empty when none bound yet).
type actionItemEventPayload struct {
	events.Base
	ActionItemID    string  `json:"action_item_id"`
	TenantID        string  `json:"tenant_id"`
	IntimationID    string  `json:"intimation_id"`
	Tipo            string  `json:"tipo"`
	GeraPeca        bool    `json:"gera_peca"`
	PieceProfileKey *string `json:"piece_profile_key"`
	DeadlineID      *string `json:"deadline_id"`
}

func newActionItemPayload(a *ActionItem) actionItemEventPayload {
	return actionItemEventPayload{
		Base:            events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: a.ID},
		ActionItemID:    a.ID,
		TenantID:        a.TenantID,
		IntimationID:    a.IntimationID,
		Tipo:            a.Tipo,
		GeraPeca:        a.GeraPeca,
		PieceProfileKey: textToNull(a.PieceProfileKey),
		DeadlineID:      textToNull(a.DeadlineID),
	}
}

// ActionItemCreated — see TypeActionItemCreated.
type ActionItemCreated struct{ actionItemEventPayload }

var _ events.Event = ActionItemCreated{}

func (ActionItemCreated) Type() string          { return TypeActionItemCreated }
func (ActionItemCreated) AggregateType() string { return aggregateTypeActionItem }

func newActionItemCreated(a *ActionItem) ActionItemCreated {
	return ActionItemCreated{newActionItemPayload(a)}
}

// ActionItemConfirmed — see TypeActionItemConfirmed.
type ActionItemConfirmed struct{ actionItemEventPayload }

var _ events.Event = ActionItemConfirmed{}

func (ActionItemConfirmed) Type() string          { return TypeActionItemConfirmed }
func (ActionItemConfirmed) AggregateType() string { return aggregateTypeActionItem }

func newActionItemConfirmed(a *ActionItem) ActionItemConfirmed {
	return ActionItemConfirmed{newActionItemPayload(a)}
}

// ActionItemDiscarded — see TypeActionItemDiscarded.
type ActionItemDiscarded struct{ actionItemEventPayload }

var _ events.Event = ActionItemDiscarded{}

func (ActionItemDiscarded) Type() string          { return TypeActionItemDiscarded }
func (ActionItemDiscarded) AggregateType() string { return aggregateTypeActionItem }

func newActionItemDiscarded(a *ActionItem) ActionItemDiscarded {
	return ActionItemDiscarded{newActionItemPayload(a)}
}

// TypeActionItemReclassified is emitted by Reclassificar (fatia 5, docs §7 questão 4): the
// advogado overrode the providência's tipo/piece_profile_key, tipo_origem now "manual". It
// carries the SAME payload shape as ActionItemCreated/ActionItemConfirmed (no new field
// needed) — internal/draft's listener resolves the affected task via its own cross-slice
// read (action_item.task_id), keyed off ActionItemID.
const TypeActionItemReclassified = "actionitem.reclassified"

// ActionItemReclassified — see TypeActionItemReclassified.
type ActionItemReclassified struct{ actionItemEventPayload }

var _ events.Event = ActionItemReclassified{}

func (ActionItemReclassified) Type() string          { return TypeActionItemReclassified }
func (ActionItemReclassified) AggregateType() string { return aggregateTypeActionItem }

func newActionItemReclassified(a *ActionItem) ActionItemReclassified {
	return ActionItemReclassified{newActionItemPayload(a)}
}

// TypeTaskCreated is the dotted id this slice CONSUMES from deadline — the OTHER half of the
// providência→tarefa loop (docs/erd-costura-providencia-tarefa-peca.md §2/§6, fatia 3):
// deadline's listener reacts to TypeActionItemCreated/TypeActionItemConfirmed above by
// creating a task and announcing it here; this slice writes the reverse pointer
// (action_item.task_id) on ITS OWN table when the event carries an action_item_id.
//
// This is a STRING LITERAL, not deadline.TypeTaskCreated: internal/deadline already imports
// internal/actionitem (for the two consts above), so importing internal/deadline here would
// create an import cycle. The literal is guarded against drift by a round-trip contract test
// (events_test.go) that DOES import internal/deadline — safe from a _test.go file, since
// deadline's own non-test code never depends on actionitem's tests, so no cycle exists at the
// production build graph level.
const TypeTaskCreated = "task.created"

// TaskCreated is the LOCAL decode shape of deadline.task.created: the subset this slice's
// listener reads. TenantID scopes the RLS-bound tx the write needs (every write in this repo
// runs tenant-scoped, CLAUDE.md inegociável) — it rides on every task.created regardless of
// origin, mirroring every other cross-slice event this repo consumes. ActionItemID is "" for
// a manual/avulsa task (deadline's TaskCreated.ActionItemID is `omitempty`) — OnTaskCreated
// skips those, since the vast majority of task.created events are NOT this origin.
type TaskCreated struct {
	events.Base
	TaskID       string `json:"task_id"`
	TenantID     string `json:"tenant_id"`
	ActionItemID string `json:"action_item_id"`
}
