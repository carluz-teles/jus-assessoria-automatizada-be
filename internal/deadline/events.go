package deadline

import (
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/events"
)

// TypeIntimationObserved is the dotted id this slice CONSUMES. Only the const crosses
// the boundary from acquisition (the producer); the payload SHAPE is redefined LOCALLY
// as IntimationObserved below, so this slice never imports acquisition's event struct.
// A contract round-trip test (deadline_events_test.go) marshals the producer's struct
// and unmarshals it here, guarding against silent field drift.
const TypeIntimationObserved = acquisition.TypeIntimationObserved

// IntimationObserved is the LOCAL decode shape of acquisition.intimation.observed: the
// exact subset of fields this slice reads to derive a prazo. UF is DENORMALIZED by the
// producer (derived from Court) so this slice never resolves it. Type carries the DJEN
// kind (CITACAO/INTIMACAO/COMUNICACAO) — the plain field, not the events.Event method:
// this struct is only ever DECODED here (events.Decode needs no interface), so the JSON
// tag "type" round-trips while the Go name stays clear. DeadlineStartAt is the wire date
// (2006-01-02), the anchor of the calendar math. Base yields the event id for dedup.
type IntimationObserved struct {
	events.Base
	TenantID        string `json:"tenant_id"`
	IntimationID    string `json:"intimation_id"`
	CourtRecordID   string `json:"court_record_id"`
	CaseID          string `json:"case_id"`
	Type            string `json:"type"`
	Court           string `json:"court"`
	UF              string `json:"uf"`
	DeadlineStartAt string `json:"deadline_start_at"`
}

// TypeDeadlineOpened is the dotted id this slice PRODUCES when a prazo is derived. Its
// "deadline" prefix routes it to the ingestao/default work at the relay; downstream
// slices (read models, reminders) consume it.
const TypeDeadlineOpened = "deadline.opened"

const aggregateTypeDeadline = "deadline"

// DeadlineOpened announces a freshly derived prazo (born PENDING). It carries what a
// consumer needs without reading back the deadline row: the prazo and its origin ids,
// the legible kind, the computed EndDate (wire date 2006-01-02) and the counting. The
// aggregate is the deadline, so its stream orders by the deadline id; Base carries the
// event id (consumer dedup) and that aggregate id.
type DeadlineOpened struct {
	events.Base
	DeadlineID    string `json:"deadline_id"`
	CourtRecordID string `json:"court_record_id"`
	IntimationID  string `json:"intimation_id"`
	Kind          string `json:"kind"`
	EndDate       string `json:"end_date"`
	Counting      string `json:"counting"`
}

var _ events.Event = DeadlineOpened{}

func (DeadlineOpened) Type() string          { return TypeDeadlineOpened }
func (DeadlineOpened) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineOpened builds the produced event from the persisted prazo. aggregate_id is
// the deadline id (a uuid, satisfying the outbox's uuid NOT NULL); the event id is a
// fresh uuid v7 (the consumer dedup key), mirroring how acquisition mints its ids.
func newDeadlineOpened(d *Deadline) DeadlineOpened {
	return DeadlineOpened{
		Base:          events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: d.ID},
		DeadlineID:    d.ID,
		CourtRecordID: d.CourtRecordID,
		IntimationID:  d.IntimationID,
		Kind:          d.Kind,
		EndDate:       d.EndDate.Format(time.DateOnly),
		Counting:      string(d.Counting),
	}
}
