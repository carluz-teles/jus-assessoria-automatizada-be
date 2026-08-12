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

// TypeIntimationCancelled is the SECOND dotted id this slice CONSUMES: the retraction
// counterpart of intimation.observed (the DJEN brought a data_cancelamento). As with the
// observed type, only the const crosses the boundary from acquisition; the payload SHAPE
// is redefined LOCALLY as IntimationCancelled below, so this slice never imports the
// producer's struct. A round-trip test (deadline_events_test.go) guards the shape.
const TypeIntimationCancelled = acquisition.TypeIntimationCancelled

// IntimationCancelled is the LOCAL decode shape of acquisition.intimation.cancelled: the
// exact subset the revocation reads. IntimationID keys the 1:1 deadline (the
// notification_id column); Reason is the DJEN motivo (empty when it did not disclose one)
// and rides through onto deadline.revoked for the audit. This struct is only ever DECODED
// here (events.Decode needs no interface), so it carries no Type()/AggregateType(). Base
// yields the event id for dedup.
type IntimationCancelled struct {
	events.Base
	TenantID     string `json:"tenant_id"`
	IntimationID string `json:"intimation_id"`
	Reason       string `json:"reason"`
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

// TypeDeadlineRevoked is the dotted id this slice PRODUCES when a prazo is revoked because
// its source intimação was retracted (ERD §7: deadline.revoked {deadline_id}; §11: uma
// intimação retificada vira prazo-fantasma). Same "deadline" prefix/routing as
// deadline.opened; downstream read models drop the prazo from the tela on it.
const TypeDeadlineRevoked = "deadline.revoked"

// DeadlineRevoked announces a derived prazo cancelled (status CANCELLED) because the
// intimação it hung on was retracted. Beyond the ERD's minimal {deadline_id} it carries
// the triggering IntimationID and the DJEN Reason (may be "") so a consumer need not read
// the row back to know why it vanished. The aggregate is the deadline, so its stream
// orders by the deadline id; Base carries the event id (consumer dedup) and that aggregate.
type DeadlineRevoked struct {
	events.Base
	DeadlineID   string `json:"deadline_id"`
	IntimationID string `json:"intimation_id"`
	Reason       string `json:"reason"`
}

var _ events.Event = DeadlineRevoked{}

func (DeadlineRevoked) Type() string          { return TypeDeadlineRevoked }
func (DeadlineRevoked) AggregateType() string { return aggregateTypeDeadline }

// newDeadlineRevoked builds the produced event from the revoked prazo's id and the
// triggering cancellation. aggregate_id is the deadline id (a uuid, satisfying the
// outbox's uuid NOT NULL); the event id is a fresh uuid v7 (the consumer dedup key),
// mirroring newDeadlineOpened.
func newDeadlineRevoked(deadlineID, intimationID, reason string) DeadlineRevoked {
	return DeadlineRevoked{
		Base:         events.Base{EventID: uuid.Must(uuid.NewV7()).String(), Aggregate: deadlineID},
		DeadlineID:   deadlineID,
		IntimationID: intimationID,
		Reason:       reason,
	}
}
