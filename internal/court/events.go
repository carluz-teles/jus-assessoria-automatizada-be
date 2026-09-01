package court

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/lib/events"
)

// TypeCourtRecordObserved aliases acquisition's own type id (the event CONTRACT is
// fair game to import across slices; the entity/repo behind it is not). The PAYLOAD
// shape below is a LOCAL redefinition matching the wire JSON, not acquisition's Go
// struct — same decoupling internal/deadline's own consumer of this event uses, so
// this slice never breaks just because acquisition's struct grows an unrelated field.
const TypeCourtRecordObserved = acquisition.TypeCourtRecordObserved

// courtRecordObserved is this slice's LOCAL view of acquisition.CourtRecordObserved:
// only the fields the arrival trigger actually needs (see OnCourtRecordObserved).
type courtRecordObserved struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	CourtRecordID string `json:"court_record_id"`
	CNJNumber     string `json:"cnj_number"`
	Court         string `json:"court"`
}

// court.connection_state_changed — the ERD's own name (§8) for the event the FE
// reacts to (polling or a live subscription) after Connect resolves asynchronously.
// Carries only the new status, never a secret or the error's full detail beyond a
// short human-readable message.

const (
	aggregateTypeCourtConnection = "court_connection"
	typeConnectionStateChanged   = "court.connection_state_changed"
)

type connectionStateChanged struct {
	events.Base
	TenantID     string `json:"tenant_id"`
	ConnectionID string `json:"connection_id"`
	Status       string `json:"status"`
}

// newConnectionStateChanged mints a FRESH event id per call (uuid v4, not a
// deterministic fact-derived key) — unlike a one-time terminal fact (certificate
// revoked), a connection's status can legitimately change to the SAME value many
// times over its life (e.g. ERROR → retry → ERROR again), and each occurrence is a
// distinct notification worth delivering, not a duplicate to dedup away.
func newConnectionStateChanged(tenantID, connectionID string, status Status) connectionStateChanged {
	return connectionStateChanged{
		Base:         events.Base{EventID: uuid.NewString(), Aggregate: connectionID},
		TenantID:     tenantID,
		ConnectionID: connectionID,
		Status:       string(status),
	}
}

func (connectionStateChanged) AggregateType() string { return aggregateTypeCourtConnection }
func (connectionStateChanged) Type() string          { return typeConnectionStateChanged }

// court.fetch_autos_requested — the self-re-enqueuing FetchAutosBatch job for one
// court_connection. SAME shape/reasoning as acquisition's EnrichmentBatchRequested
// (internal/acquisition/events.go): Step mints a FRESH EventID per re-enqueue,
// because the relay's asynq TaskID is unique GLOBAL per event id — reusing the
// CURRENT step's id for its own continuation would collide with itself while still
// "active" in asynq's bookkeeping and get silently dropped. Step 0 (the arrival
// trigger, published by OnCourtRecordObserved) uses a STABLE id instead — exactly
// the burst-collapse behavior wanted there: several court_record_observed events
// for the same connection in a short window fold into one pending task.
type fetchAutosRequested struct {
	events.Base
	TenantID     string `json:"tenant_id"`
	ConnectionID string `json:"connection_id"`
	Step         int    `json:"step"`
	processAt    time.Time
}

const (
	typeFetchAutosRequested = "court.fetch_autos_requested"
	fetchAutosContinueDelay = 2 * time.Second
)

var (
	_ events.Event          = fetchAutosRequested{}
	_ events.ScheduledEvent = fetchAutosRequested{}
)

func (fetchAutosRequested) Type() string          { return typeFetchAutosRequested }
func (fetchAutosRequested) AggregateType() string { return aggregateTypeCourtConnection }

// ProcessAt: step 0 delivers immediately (zero processAt); every continuation
// carries now+carência so the previous step's writes commit first (repo directive:
// scheduled work is an asynq scheduled task, never a polling loop).
func (e fetchAutosRequested) ProcessAt() (time.Time, bool) {
	return e.processAt, !e.processAt.IsZero()
}

// newFetchAutosRequested builds the ARRIVAL-TRIGGER step (step 0, stable id, burst
// collapse) — published by OnCourtRecordObserved whenever a matching, CONNECTED
// court_connection exists.
func newFetchAutosRequested(tenantID, connectionID string) fetchAutosRequested {
	return fetchAutosRequested{
		Base:         events.Base{EventID: fetchAutosRequestedEventID(connectionID, 0), Aggregate: connectionID},
		TenantID:     tenantID,
		ConnectionID: connectionID,
		Step:         0,
	}
}

// nextFetchAutosStep builds the CONTINUATION step from the current one: a fresh
// EventID (step+1) and ProcessAt=now+carência.
func (e fetchAutosRequested) nextFetchAutosStep(at time.Time) fetchAutosRequested {
	return fetchAutosRequested{
		Base:         events.Base{EventID: fetchAutosRequestedEventID(e.ConnectionID, e.Step+1), Aggregate: e.ConnectionID},
		TenantID:     e.TenantID,
		ConnectionID: e.ConnectionID,
		Step:         e.Step + 1,
		processAt:    at.Add(fetchAutosContinueDelay),
	}
}

func fetchAutosRequestedEventID(connectionID string, step int) string {
	return fmt.Sprintf("fetch-autos:%s:%d", connectionID, step)
}

// court.fetch_autos_item_requested — an individual-record retry after
// FetchAutosBatch left it due because of a TRANSIENT fault (see BatchResult's
// doc). Deliberately NOT a ScheduledEvent: the relay's default MaxRetry (5 for any
// "court.*" event — see lib/events/relay.go's maxRetryFor, the default bucket) and
// asynq's own backoff between attempts are what bound the retries here, not a
// bespoke retry_count column. The EventID is STABLE per (connection, record): if
// the SAME item is still due by the time this fires, redelivery naturally
// collapses instead of piling up duplicate pending retries for it.
type fetchAutosItemRequested struct {
	events.Base
	TenantID      string `json:"tenant_id"`
	ConnectionID  string `json:"connection_id"`
	CourtRecordID string `json:"court_record_id"`
	CNJNumber     string `json:"cnj_number"`
}

var _ events.Event = fetchAutosItemRequested{}

func (fetchAutosItemRequested) Type() string          { return typeFetchAutosItemRequested }
func (fetchAutosItemRequested) AggregateType() string { return aggregateTypeCourtConnection }

const typeFetchAutosItemRequested = "court.fetch_autos_item_requested"

func newFetchAutosItemRequested(tenantID, connectionID string, item FetchStateItem) fetchAutosItemRequested {
	return fetchAutosItemRequested{
		Base:          events.Base{EventID: fetchAutosItemEventID(connectionID, item.CourtRecordID), Aggregate: connectionID},
		TenantID:      tenantID,
		ConnectionID:  connectionID,
		CourtRecordID: item.CourtRecordID,
		CNJNumber:     item.CNJNumber,
	}
}

func fetchAutosItemEventID(connectionID, courtRecordID string) string {
	return fmt.Sprintf("fetch-autos-item:%s:%s", connectionID, courtRecordID)
}
