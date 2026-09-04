// Package deadline is the prazos slice: the CREATION path of a legal deadline. Its
// listener consumes acquisition.intimation.observed, derives the prazo
// DETERMINISTICALLY (rules layer → lib/calendar), and persists it born PENDING —
// or already OPEN when confirmacao_exigida is false (the system assumed it, emitting
// deadline.assumed alongside) — while always emitting deadline.opened, all in one
// idempotent, tenant-scoped transaction.
//
// It is a vertical slice: it talks to acquisition ONLY by event contract (it imports
// the produced type's const, never acquisition's entity/repo), and it never touches
// another slice's tables beyond the read of court_record.class it needs for the rito.
package deadline

import "time"

// Deadline is the legal countdown derived from an intimação — the product's core fact
// (docs/erd-prazos.md §1). It anchors on a court_record (FK) and is 1:1 with the
// intimação (the notification_id UNIQUE column). A rule-derived prazo is BORN PENDING
// (a suggestion awaiting the human F2 confirmation) UNLESS the system already decided
// no confirmation is needed (ConfirmacaoExigida=false), in which case it is BORN OPEN
// directly — counting, entering alerts, without waiting on a manual click. Either way,
// the calendar math (EndDate, HolidaysApplied) is deterministic and auditable, so
// "por que dia 14 e não 12?" is answerable from the row.
//
// entity.go holds only the aggregate and its value types — it imports no repository,
// listener, or lib (the slice's inward dependency rule).
type Deadline struct {
	ID            string
	TenantID      string
	CourtRecordID string
	// IntimationID is persisted in the deadline.notification_id column: that column
	// keeps its pre-rename name but references intimation(id) (migration 0006). The
	// mapper documents the quirk; the domain speaks IntimationID.
	IntimationID    string
	Kind            string
	Days            int
	Counting        Counting
	Doubled         bool
	DoubledReason   string
	HolidaysApplied []time.Time
	StartDate       time.Time
	EndDate         time.Time
	// PrazoInterno is the internal safety buffer — internalBufferBusinessDays (2) business
	// days before EndDate — recomputed at the same 4 points EndDate is (birth, confirm,
	// adjust, and the aceita_declarado apuração). Persisted (deadline.prazo_interno), not a
	// read-time placeholder.
	PrazoInterno time.Time
	Status       Status
	Source       Source
	RulesVersion string
	// AnchorEvent is which observed intimação date the start_date was anchored on. The creation
	// path is born on the derived deadline_start_at (AnchorDeadlineStart); the confirmation panel
	// may re-anchor it later.
	AnchorEvent AnchorEvent
	// LegalCitation is the frozen citation snapshot (e.g. "art. 218, §1º, CPC") copied from the
	// resolved deadline_rule at derivation — distinct from DoubledReason. "" when the rule has no
	// citation (the catch-all / generic rules).
	LegalCitation string
	// V1 fields — precedência de fontes, selo de confiança e proveniência
	Origem             Origem     // precedência de fontes (declarado > calculado > ia)
	Seal               Seal       // confiança ortogonal ao estado
	ConfirmacaoExigida bool       // true se seal=A_APURAR OU política estrita
	Providencia        string     // tipo de ato (parte da chave 1:N)
	ConfirmedBy        *string    // nullable, quem confirmou
	ConfirmedAt        *time.Time // nullable, quando confirmou
}

// CalcMemory stores the deterministic calculation provenance (V1). It answers "por
// que essa data?" — the auditable trail of every number that fed the computation.
// Persisted snapshot, never recomputed (the calendar provider may change).
type CalcMemory struct {
	ID                      string
	TenantID                string
	DeadlineID              string
	PrazoBase               string
	PrazoBaseFonte          string
	TermoInicialRegra       string
	DiasUteis               bool
	DobraMotivo             string
	TabelaLegalRef          string
	IATipoInferido          string
	IAConfianca             float64
	CalendarProviderVersion string
}

// AppliedHoliday is a snapshot of a feriado applied to a calculation (V1). The
// calendar is a licensed external service; we persist what was applied, not the
// source — proveniência, not ownership.
type AppliedHoliday struct {
	ID           string
	TenantID     string
	CalcMemoryID string
	Data         time.Time
	Nome         string
	Ambito       string
	Comarca      string
}

// CrossValidation records declared vs calculated validation (V1). Optional — only
// exists when there is a declared date AND a calculated date. The divergence
// resolution is persisted with who decided.
type CrossValidation struct {
	ID            string
	TenantID      string
	DeadlineID    string
	DataDeclarada time.Time
	DataCalculada time.Time
	DifDias       int
	Resultado     string // convergente | divergente
	CausaProvavel string
	Decisao       string // aceita_declarado | aceita_calculado | ajuste_manual
	DecididoPor   string
}

// DeadlineEvent is one row in the audit trail (V1). Append-only — recálculo por
// movimento superveniente never overwrites; it adds a new event. History is auditable.
type DeadlineEvent struct {
	ID         string
	TenantID   string
	DeadlineID string
	Tipo       string // calculado | assumido | validado | confirmado | recalculado | em_risco | cumprido | override
	Detalhe    string
	AtorID     string
	Em         time.Time
}

// DeadlinePolicy is the per-tenant confirmation policy (V1). The default is seletiva
// (ConfirmacaoObrigatoria=false): system assumes confiável deadlines; IA and
// divergent always require human (the non-negotiable floor). Strict mode
// (ConfirmacaoObrigatoria=true) raises the bar for ALL deadlines.
type DeadlinePolicy struct {
	TenantID               string
	ConfirmacaoObrigatoria bool
}

// Counting is how the days are counted. Cível/CPC counts in dias úteis (art. 219);
// some ritos (trabalhista/CLT) count corrido. The value drives which lib/calendar
// motor computes EndDate.
type Counting string

const (
	CountingBusiness Counting = "BUSINESS"
	CountingCalendar Counting = "CALENDAR"
)

// Status is the prazo lifecycle, a closed set the DB CHECK (0024) also enforces. A
// rule-derived prazo is born PENDING (awaiting human confirmation) UNLESS
// confirmacao_exigida is already false (seal=confiavel, tenant policy seletiva) — the
// system already decided no human is needed, so it is born OPEN directly, skipping the
// PENDING→OPEN manual confirm step. A prazo born already past its D+1 carência is born
// MISSED instead, regardless of confirmacao_exigida (see OnIntimationObserved). The
// remaining transitions (revocation/expiry/met) are later slices.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusOpen      Status = "OPEN"
	StatusMet       Status = "MET"
	StatusMissed    Status = "MISSED"
	StatusCancelled Status = "CANCELLED"
	// StatusNoDeadline is "mera ciência": the lawyer declared the intimação carries no prazo
	// to cumprir. It is DISTINCT from CANCELLED (revocation by a retificação event): a human
	// PENDING|OPEN → NO_DEADLINE decision, reversible via reopen (→ PENDING). A NO_DEADLINE
	// prazo sits OUTSIDE the MarkMissed (status='OPEN') and reconcile (MISSED,OPEN) guards, so
	// it never auto-flips to MISSED nor gets resurrected.
	StatusNoDeadline Status = "NO_DEADLINE"
	// StatusResolvedOnConclusion is the Achado 2 terminal state (fatia 2b, migration 0098):
	// a PENDING/OPEN/MISSED prazo auto-resolved because its court_record concluded (lifecycle
	// → ARCHIVED). DISTINCT from CANCELLED (a retificação-driven revocation, deadline slice's
	// own event) and from NO_DEADLINE (a human "mera ciência" declaration): this transition is
	// system-driven by a DIFFERENT slice's fact (acquisition.court_record_archived) and must
	// stay auditable in the deadline_event trail as "resolvido por conclusão do processo", not
	// collapse into a generic cancellation. Irreversible in v0 (no reopen path, unlike
	// NO_DEADLINE) — the process concluding is not expected to un-conclude.
	StatusResolvedOnConclusion Status = "RESOLVED_ON_CONCLUSION"
)

// AnchorEvent is which observed date of the intimação anchors the prazo's start_date (a closed
// set the DB CHECK also enforces). DEADLINE_START is the legacy default (the derived
// deadline_start_at); MADE_AVAILABLE / PUBLISHED re-anchor on the intimação's real dates. The
// confirmation panel lets the lawyer re-count from a different termo inicial.
type AnchorEvent string

const (
	AnchorMadeAvailable AnchorEvent = "MADE_AVAILABLE"
	AnchorPublished     AnchorEvent = "PUBLISHED"
	AnchorDeadlineStart AnchorEvent = "DEADLINE_START"
)

// validAnchorEvent reports whether a is a member of the closed anchor set (the same set the DB
// CHECK enforces). The empty string is NOT accepted here — the edge defaults an absent
// anchor_event to DEADLINE_START before it reaches the domain.
func validAnchorEvent(a AnchorEvent) bool {
	switch a {
	case AnchorMadeAvailable, AnchorPublished, AnchorDeadlineStart:
		return true
	}
	return false
}

// IntimationAnchors are the three observed dates of an intimação the confirmation panel can
// re-anchor a prazo on (GetIntimationAnchors). All three are NOT NULL date columns on the
// intimation row, so a present intimação always yields three real dates; the use case maps the
// chosen AnchorEvent to one of them for the recompute. A missing intimação for the tenant is
// ErrDeadlineNotFound (the prazo it anchors could not be found), never a zero value.
type IntimationAnchors struct {
	MadeAvailableAt time.Time
	PublishedAt     time.Time
	DeadlineStartAt time.Time
}

// startFor maps an AnchorEvent to the matching observed date. DEADLINE_START (the default and
// any unrecognized value, defensively) yields the derived deadline_start_at — the legacy anchor
// the prazo was born on.
func (a IntimationAnchors) startFor(anchor AnchorEvent) time.Time {
	switch anchor {
	case AnchorMadeAvailable:
		return a.MadeAvailableAt
	case AnchorPublished:
		return a.PublishedAt
	default:
		return a.DeadlineStartAt
	}
}

// Source records where the {days, counting} came from. The creation path derives from
// the conservative rules layer (RULE); the F2 confirmation creates its tasks MANUAL.
// AI is a later slice.
type Source string

const (
	SourceRule   Source = "RULE"
	SourceAI     Source = "AI"
	SourceManual Source = "MANUAL"
)

// Origem records the precedência de fontes (V1). Replaces Source semantically:
// declarado > calculado > ia. The hierarchy of origin is the hierarchy of risk.
type Origem string

const (
	OrigemDeclarado  Origem = "declarado"
	OrigemValidado   Origem = "validado"
	OrigemCalculado  Origem = "calculado"
	OrigemDivergente Origem = "divergente"
	OrigemIA         Origem = "ia"
	OrigemManual     Origem = "manual"
)

// Seal is the confidence seal, orthogonal to the operational state (V1). A deadline
// can be ACTIVE and COUNTING while still requiring human confirmation.
type Seal string

const (
	SealConfiavel Seal = "confiavel"
	SealAApurar   Seal = "a_apurar"
)

// Task is one actionable work item (docs/erd-prazos.md §4/§10) — the checklist of steps
// toward the legal prazo. 1 legal prazo (Deadline) → N tasks; a task can also be avulsa
// (POST /v1/tasks, no deadline). The assignee lives on the task, not the prazo (the prazo
// is the fact, the task is who does it). Tasks are BORN OPEN (at F2 confirmation or via the
// manual CREATE) and move OPEN→DONE / OPEN→DISMISSED via the task write path (5b); the
// creation source is MANUAL here (RULE/AI are later slices). entity.go holds only the
// aggregate + value types (no repo/lib import).
type Task struct {
	ID             string
	TenantID       string
	CourtRecordID  string
	DeadlineID     string
	IntimationID   string
	Title          string
	Description    string
	Kind           string     // the TaskKind taxonomy (ANALISE|PECA|…); "" = uncategorized
	DueDate        *time.Time // optional own date (≤ Deadline.EndDate when present)
	Status         TaskStatus
	Source         Source
	Priority       string // the TaskPriority triage flag (HIGH|MEDIUM|LOW); "" = sem prioridade
	AssigneeUserID string // optional responsável ("meus prazos")
	CreatedBy      string
	CompletedAt    *time.Time // stamped when the task is marked DONE; NULL while OPEN/DISMISSED
	// ActionItemID is "" for a manual/avulsa task (POST /v1/tasks) and set only when the task
	// was born automatically from a confiável providência (docs/erd-costura-providencia-
	// tarefa-peca.md §2/§6, fatia 3: actionitem.created/confirmed → task). migration 0087's
	// UNIQUE constraint on the column is the idempotency floor InsertTask's ON CONFLICT relies
	// on.
	ActionItemID string
}

// TaskStatus is the task lifecycle, a closed set the DB CHECK (0024) also enforces. A
// task is born OPEN; the OPEN→DONE / OPEN→DISMISSED transitions are the task write path
// (5b, POST /v1/tasks/:id/done | .../dismiss).
type TaskStatus string

const (
	TaskStatusOpen      TaskStatus = "OPEN"
	TaskStatusDone      TaskStatus = "DONE"
	TaskStatusDismissed TaskStatus = "DISMISSED"
)

// TaskKind is the task category — the SINGLE source of truth for the taxonomy the
// advisory sugere (internal/advisory prompt: ANALISE|PECA|PROTOCOLO|PROVIDENCIA|CIENCIA)
// and the edge validation of task.kind (validation.go) both enforce. A task may carry no
// kind at all (an avulsa/uncategorized task — kind is NULL in the DB), so the empty string
// is legal; any NON-empty kind must be one of these. The enum is text + app validation,
// mirroring the deadline kinds above (no DB CHECK, per the project convention).
type TaskKind string

const (
	TaskKindAnalise     TaskKind = "ANALISE"
	TaskKindPeca        TaskKind = "PECA"
	TaskKindProtocolo   TaskKind = "PROTOCOLO"
	TaskKindProvidencia TaskKind = "PROVIDENCIA"
	TaskKindCiencia     TaskKind = "CIENCIA"
)

// validTaskKind reports whether a task kind is acceptable: one of the closed set, or the empty
// kind (an uncategorized task — kind is NULL in the DB). The caller decides whether it wants to
// require a kind at all; the edge rules treat "" as a no-op.
func validTaskKind(k string) bool {
	if k == "" {
		return true
	}
	switch TaskKind(k) {
	case TaskKindAnalise, TaskKindPeca, TaskKindProtocolo, TaskKindProvidencia, TaskKindCiencia:
		return true
	}
	return false
}

// TaskPriority is the triage flag a user pins on a task (docs/erd-prazos.md §4/§10, the Tarefa
// detail's "Prioridade" property) — HIGH|MEDIUM|LOW, or none at all ("sem prioridade" is the
// default, priority NULL in the DB). Like TaskKind it is text + app validation (validation.go)
// + a DB CHECK (0053) for the closed non-empty set; the empty string is legal and means unset.
type TaskPriority string

const (
	TaskPriorityHigh   TaskPriority = "HIGH"
	TaskPriorityMedium TaskPriority = "MEDIUM"
	TaskPriorityLow    TaskPriority = "LOW"
)

// validTaskPriority reports whether a task priority is acceptable: one of the closed set, or the
// empty priority ("sem prioridade" — priority is NULL in the DB). Mirrors validTaskKind: any
// NON-empty value must be one of HIGH|MEDIUM|LOW; "" is a no-op the edge rules accept.
func validTaskPriority(p string) bool {
	if p == "" {
		return true
	}
	switch TaskPriority(p) {
	case TaskPriorityHigh, TaskPriorityMedium, TaskPriorityLow:
		return true
	}
	return false
}

// TaskItem is one checklist step of a task (docs/erd-prazos.md §4/§10, the Tarefas screen):
// a small, orderable, tickable subtarefa ("Ler intimação", "Redigir", …). 1 task → N items
// (task_item, migration 0031, ON DELETE CASCADE). Position orders the checklist; Done is the
// tick, DoneAt stamped when it flips true and cleared when it flips false. entity.go holds only
// the aggregate + value types (no repo/lib import).
type TaskItem struct {
	ID        string
	TenantID  string
	TaskID    string
	Title     string
	Position  int
	Done      bool
	DoneAt    *time.Time
	CreatedAt time.Time
}

// TaskComment is one message in a task's discussion thread (docs/erd-prazos.md §4/§10, the Tarefa
// detail's "Comentários" tab). 1 task → N comments (task_comment, migration 0054, ON DELETE
// CASCADE). AuthorUserID is the internal app_user id of the writer (the verified principal, never
// the body). CreatedAt orders the thread (oldest-first in the detail view). entity.go holds only
// the aggregate + value types (no repo/lib import).
type TaskComment struct {
	ID           string
	TenantID     string
	TaskID       string
	AuthorUserID string
	Body         string
	CreatedAt    time.Time
}

// ActivityEventType is the closed set of task_activity event kinds (docs/erd-prazos.md §4/§10, the
// Tarefa detail's "Atividade" tab) — one per meaningful mutation of a task. Like TaskKind/
// TaskPriority it is text + app validation (the DB column is plain text; the closed set lives
// here). A field-change event (TITLE_CHANGED, …) carries from/to; a create/lifecycle/comment
// event leaves them NULL.
type ActivityEventType string

const (
	ActivityTaskCreated        ActivityEventType = "TASK_CREATED"
	ActivityTitleChanged       ActivityEventType = "TITLE_CHANGED"
	ActivityDescriptionChanged ActivityEventType = "DESCRIPTION_CHANGED"
	ActivityKindChanged        ActivityEventType = "KIND_CHANGED"
	ActivityPriorityChanged    ActivityEventType = "PRIORITY_CHANGED"
	ActivityDueDateChanged     ActivityEventType = "DUE_DATE_CHANGED"
	ActivityAssigneeChanged    ActivityEventType = "ASSIGNEE_CHANGED"
	ActivityTaskDone           ActivityEventType = "TASK_DONE"
	ActivityTaskDismissed      ActivityEventType = "TASK_DISMISSED"
	ActivityCommented          ActivityEventType = "COMMENTED"
)

// TaskActivity is one row of a task's audit log (docs/erd-prazos.md §4/§10, the Tarefa detail's
// "Atividade" tab): a meaningful mutation appended in the SAME tx as the mutation itself. 1 task →
// N activity rows (task_activity, migration 0055, ON DELETE CASCADE). ActorUserID is the app_user
// id that caused the event (the verified principal). From/To carry the "de X para Y" a field
// change renders (both nil/"" for a create/lifecycle/comment). CreatedAt orders the log
// (newest-first in the detail view).
type TaskActivity struct {
	ID          string
	TenantID    string
	TaskID      string
	ActorUserID string
	EventType   ActivityEventType
	FromValue   string
	ToValue     string
	CreatedAt   time.Time
}

// TaskProgress is a task's checklist tally — Done ticked of Total items. It feeds both the
// detail view's progress bar and the derived DisplayStatus (any item done ⇒ "Em execução").
// An empty checklist is {0, 0}.
type TaskProgress struct {
	Done  int `json:"done"`
	Total int `json:"total"`
}

// DisplayStatus is the presentation status the Tarefas screen buckets a task by — DERIVED
// (not stored) from the task's lifecycle status, its checklist progress, and its due_date.
// It is a closed set of Portuguese labels the FE renders directly. A DISMISSED task has NO
// display bucket (it is dispensada, out of the cockpit), so DisplayStatus returns "" for it.
type DisplayStatus string

const (
	DisplayAberta     DisplayStatus = "Aberta"
	DisplayEmExecucao DisplayStatus = "Em execução"
	DisplayConcluida  DisplayStatus = "Concluída"
	DisplayAtrasada   DisplayStatus = "Atrasada"
)

// deriveDisplayStatus is the SINGLE source of truth for a task's presentation status, reused
// by the detail view, the list rows and the tasks/summary buckets — so a task shows the same
// status everywhere. The rules (docs: the Tarefas screen), evaluated in order:
//   - DONE                          → Concluída (a completion trumps everything);
//   - OPEN & due_date < today       → Atrasada (overdue beats in-progress);
//   - OPEN & some checklist item done → Em execução;
//   - OPEN & no item done            → Aberta;
//   - DISMISSED                       → "" (no bucket — dispensada is out of the cockpit).
//
// `now` is the reference day (the caller passes the same clock the read model uses); only the
// calendar day matters, so a due_date is "past" strictly before today.
func deriveDisplayStatus(status TaskStatus, progress TaskProgress, dueDate *time.Time, now time.Time) DisplayStatus {
	switch status {
	case TaskStatusDone:
		return DisplayConcluida
	case TaskStatusOpen:
		if dueDate != nil && startOfDay(*dueDate).Before(startOfDay(now)) {
			return DisplayAtrasada
		}
		if progress.Done > 0 {
			return DisplayEmExecucao
		}
		return DisplayAberta
	default:
		// DISMISSED — no display bucket.
		return ""
	}
}

// DeadlineForConfirm is the thin anchor read the F2 confirmation loads BEFORE the
// recompute (GetDeadlineForConfirm), keyed by the 1:1 intimação: the prazo id, the
// record it hangs on (feeds the court lookup + the tasks), and the fixed StartDate the
// calendar math re-counts from. A missing prazo for the intimação is ErrDeadlineNotFound
// (→ 404), never a zero value.
type DeadlineForConfirm struct {
	ID            string
	CourtRecordID string
	StartDate     time.Time
	// LegalCitation is the snapshot copied at derivation; the confirm carries it forward so a
	// re-confirm never drops the fundamento the panel shows.
	LegalCitation string
}

// DeadlineForAdjust is the FULL adjustable state the F2 ajuste manual loads BEFORE the
// recompute (GetDeadlineForAdjust, PATCH /v1/prazos/:id), keyed by id: the prazo id, the
// record it hangs on (feeds the court lookup), the fixed StartDate the calendar re-counts
// from, the Status the ajuste is gated on (only PENDING/OPEN is adjustable), and the CURRENT
// {Kind, Days, Counting, Doubled, DoubledReason} the partial patch is applied over — a field
// absent from the body keeps its stored value. A missing prazo is ErrDeadlineNotFound (→ 404),
// never a zero value.
type DeadlineForAdjust struct {
	ID            string
	CourtRecordID string
	// IntimationID keys the anchor read (GetIntimationAnchors): when the ajuste re-anchors
	// (anchor_event present), the recompute re-counts from the chosen intimação date, not the
	// stored start_date.
	IntimationID    string
	StartDate       time.Time
	Status          Status
	Kind            string
	Days            int
	Counting        Counting
	Doubled         bool
	DoubledReason   string
	AnchorEvent     AnchorEvent
	ManualExtraDays int
	// Origem/Selo (V1) ride along so apurar.go's ApurarDivergencia can gate on the current selo
	// and stamp the UNCHANGED origem onto the deadline.seal_assigned event without a second
	// read — origem is immutable after creation (only selo flips).
	Origem Origem
	Selo   Seal
}

// Kind constants — the legible prazo kinds the v0 rules layer emits (docs/erd-prazos.md
// §4/§8). GENERICO is the safe catch-all the UI later flags "confirme".
const (
	KindContestacao  = "CONTESTACAO"
	KindManifestacao = "MANIFESTACAO"
	KindGenerico     = "GENERICO"
)

// RevokedDeadline is the thin result of revoking a prazo by its intimação: the id of the
// row that flipped to CANCELLED and the record it hung on. The revoke path needs only the
// id (it anchors the deadline.revoked event); CourtRecordID is carried for symmetry with
// the open path and any consumer that keys off the record. A no-op revoke — no prazo, or
// one already CANCELLED — yields no RevokedDeadline (ErrDeadlineNotFound), never a zero value.
type RevokedDeadline struct {
	ID            string
	CourtRecordID string
}

// ReconcilableDeadline is the thin read the docket-entry reconcile loads for each MISSED/OPEN
// prazo of a court_record (ListReconcilableDeadlines): the id it may flip to MET and the fixed
// StartDate the response-movement predicate compares occurred_at against (a movimento só
// cumpre o prazo se ocorreu em/depois do início da contagem). It is a read value object — the
// reconcile flips through the guarded MarkMet UPDATE, never through this struct.
type ReconcilableDeadline struct {
	ID        string
	StartDate time.Time
}

// DeadlineForCheck is the thin re-read of a prazo at a scheduled mark's fire time
// (reminder_check / missed_check): the current Status the handler branches on, the EndDate
// and the context (Kind, Counting, CourtRecordID) a lembrete or MISSED fact may carry. It
// is a read value object — the fire handlers never mutate through it (MISSED goes through
// the guarded MarkMissed UPDATE). A missing id in the tenant is ErrDeadlineNotFound.
type DeadlineForCheck struct {
	ID            string
	Status        Status
	EndDate       time.Time
	CourtRecordID string
	Kind          string
	Counting      Counting
}

// DeadlineRule is the resolved conservative rule (a deadline_rule row, §8): how many
// days, counted which way, under which kind, and whether the rule already implies the
// dobro. It is a read value object — the resolver returns the most specific active
// match for (intimation_type, court), falling back to the '*' catch-all.
type DeadlineRule struct {
	RulesVersion string
	Kind         string
	Days         int
	Counting     Counting
	Doubled      bool
	// LegalCitation is the rule's legal fundamento (deadline_rule.legal_citation, migration 0049),
	// snapshotted onto the derived prazo. "" when the rule has none (the '*' catch-all / generic).
	LegalCitation string
}
