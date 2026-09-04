package notifications

import (
	"time"

	"github.com/jusassessoria/platform/internal/acquisition"
	"github.com/jusassessoria/platform/internal/billing"
	"github.com/jusassessoria/platform/internal/deadline"
	"github.com/jusassessoria/platform/internal/draft"
	"github.com/jusassessoria/platform/lib/events"
)

// The two acquisition-produced events this slice ALSO consumes (slice 1a). Vertical
// slice: slices talk only by event contract (docs §2.5), so notifications imports the
// contract — never acquisition's entity/repo — as a type alias. The import is acyclic
// (acquisition does not import notifications). The alias keeps the listener/use case
// speaking in notifications' own names while the shape and dotted id stay
// single-sourced in acquisition (no drift).
type (
	BackfillFinished    = acquisition.BackfillFinished
	DocketEntryObserved = acquisition.DocketEntryObserved
)

const (
	TypeBackfillFinished    = acquisition.TypeBackfillFinished
	TypeDocketEntryObserved = acquisition.TypeDocketEntryObserved
)

// The two deadline-produced events this slice ALSO consumes (fatia 4c). Unlike the
// acquisition events above (aliased to the producer's struct), these follow the
// deadline slice's own consumed-event mold: only the type CONST crosses the boundary
// (TypeDeadlineDueSoon/TypeDeadlineMissed = deadline.…), while the payload SHAPE is
// redefined LOCALLY below — so this slice never imports deadline's event struct, only
// the dotted id. A contract round-trip test (events_test.go) marshals the producer's
// struct and unmarshals it into the local shape, guarding against silent field drift.
// The import is acyclic (deadline does not import notifications).
const (
	TypeDeadlineDueSoon = deadline.TypeDeadlineDueSoon
	TypeDeadlineMissed  = deadline.TypeDeadlineMissed
	// TypeDeadlineResolvedOnConclusion (Achado 2, fatia 2c) follows the SAME mold: only the
	// dotted id crosses the boundary, the payload shape is redefined locally below.
	TypeDeadlineResolvedOnConclusion = deadline.TypeDeadlineResolvedOnConclusion
)

// DeadlineDueSoon is the LOCAL decode shape of deadline.due_soon: a prazo approaching its
// vencimento. TenantID scopes the aviso (barrier 1); DeadlineID + DaysLeft are what the
// lembrete text and payload need. Only ever DECODED here (events.Decode needs no
// interface), so it carries no events.Event method. Base yields the event id for dedup.
type DeadlineDueSoon struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
	DaysLeft   int    `json:"days_left"`
}

// DeadlineMissed is the LOCAL decode shape of deadline.missed: a prazo auto-marked MISSED
// at the D+1 carência. Same tenant scope and only-ever-decoded contract as DeadlineDueSoon;
// it needs just the deadline id. Base yields the event id for dedup.
type DeadlineMissed struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
}

// DeadlineResolvedOnConclusion is the LOCAL decode shape of deadline.resolved_on_conclusion
// (Achado 2, fatia 2c): a prazo auto-resolved because its court_record concluded. Same
// tenant scope and only-ever-decoded contract as DeadlineMissed.
type DeadlineResolvedOnConclusion struct {
	events.Base
	TenantID   string `json:"tenant_id"`
	DeadlineID string `json:"deadline_id"`
}

// The billing-produced events this slice ALSO consumes (fatia 2, fatia 6b).
// TypeTrialEndingSoon follows the deadline mold below (payload shape redefined
// locally). TypePaymentFailed instead follows the acquisition mold above (type
// ALIAS to billing.PaymentFailed): its shape (TenantID/InvoiceID/AmountDue) is
// exactly what the aviso needs, so redefining it locally would just be drift risk
// with no benefit. Both imports are acyclic (billing does not import notifications).
const TypeTrialEndingSoon = billing.TypeTrialEndingSoon

type PaymentFailed = billing.PaymentFailed

const TypePaymentFailed = billing.TypePaymentFailed

// TrialEndingSoon is the LOCAL decode shape of billing.trial_ending_soon: a
// tenant's trial approaching its end. TenantID scopes the aviso (barrier 1);
// TrialEndsAt + DaysLeft are what the lembrete text needs. Only ever DECODED here
// (events.Decode needs no interface), so it carries no events.Event method. Base
// yields the event id for dedup.
type TrialEndingSoon struct {
	events.Base
	TenantID    string    `json:"tenant_id"`
	TrialEndsAt time.Time `json:"trial_ends_at"`
	DaysLeft    int       `json:"days_left"`
}

// TypeNotificationRequested is the dotted id this slice consumes. Its "notification"
// prefix routes it to the "notifications" work queue at the relay (lib/events'
// queueFor), so a slow email send never blocks court sync or AI work.
const TypeNotificationRequested = "notification.requested"

// NotificationRequested is the generic request-to-notify contract this slice
// consumes: WHO (RecipientUserID, in the tenant TenantID), WHAT template (Type,
// e.g. "member_joined") and its data (Payload). Base carries the event id (consumer
// dedup) and the aggregate id (the tenant).
//
// Type is a plain field (the template selector), not the events.Event Type() method:
// this struct is only ever DECODED here (events.Decode needs no interface), and the
// producer — a future slice — owns whatever type publishes it. Keeping Type as data
// is what lets one generic event drive every kind of aviso.
type NotificationRequested struct {
	events.Base
	TenantID        string         `json:"tenant_id"`
	RecipientUserID string         `json:"recipient_user_id"`
	Type            string         `json:"type"`
	Payload         map[string]any `json:"payload"`
}

// Os dois eventos de peticionamento automático (Fatia 1) que este slice também
// consome. Seguem o molde do deadline: só a CONST do tipo cruza a fronteira
// (TypeFilingSucceeded/TypeFilingFailed = draft.…), enquanto o SHAPE do payload é
// redefinido LOCALMENTE abaixo — assim este slice nunca importa o struct de evento
// do draft, apenas o id pontuado. Um teste de round-trip (events_test.go) marca o
// struct do produtor e desmarca no shape local, protegendo contra drift silencioso.
// O import é acíclico (draft não importa notifications).
const (
	TypeFilingSucceeded = draft.TypeFilingSucceeded
	TypeFilingFailed    = draft.TypeFilingFailed
)

// FilingSucceeded é o shape LOCAL de decode de filing.succeeded: a peça foi
// protocolada no e-SAJ. TenantID scopa o aviso (barreira 1); FilingNumber é o
// número de protocolo exibido. Base cede o event id para dedup.
type FilingSucceeded struct {
	events.Base
	TenantID        string `json:"tenant_id"`
	DraftID         string `json:"draft_id"`
	FilingAttemptID string `json:"filing_attempt_id"`
	FilingNumber    string `json:"filing_number"`
}

// FilingFailed é o shape LOCAL de decode de filing.failed: a tentativa de protocolo
// automático falhou. TenantID scopa o aviso; FailureReason é exibida para o usuário
// decidir pelo protocolo manual. Base cede o event id para dedup.
type FilingFailed struct {
	events.Base
	TenantID        string `json:"tenant_id"`
	DraftID         string `json:"draft_id"`
	FilingAttemptID string `json:"filing_attempt_id"`
	FailureReason   string `json:"failure_reason"`
}
