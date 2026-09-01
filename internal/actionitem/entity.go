// Package actionitem is the Providência slice (docs/erd-costura-providencia-tarefa-peca.md
// §2/§3): the diagnostic step between an intimação and the work it triggers. It NASCE from
// an event (acquisition.intimation.analyzed — this slice never imports acquisition's
// entity/repo) and its confirmar/descartar endpoints are its own HTTP surface. A future
// fatia's deadline listener owns turning a confiável action_item into a task (task_id is
// schema-ready here, never written by this slice).
package actionitem

import "time"

// TipoOrigem records where the providência's tipo classification came from — the same
// precedence the Motor de Prazos already uses for the prazo itself (docs §3): declarado >
// ia > manual. text + app validation (validation.go), mirroring every other closed-set
// column in the repo.
type TipoOrigem string

const (
	TipoOrigemDeclarado TipoOrigem = "declarado"
	TipoOrigemIA        TipoOrigem = "ia"
	TipoOrigemManual    TipoOrigem = "manual"
)

// TipoStatus is the confidence gate on the tipo classification: a declarado/manual tipo is
// born confiável (the system acts on it without friction); an IA-inferred tipo is born
// a_confirmar and needs a human's confirmar before anything downstream (task creation)
// treats it as settled (docs §3, "o piso").
type TipoStatus string

const (
	TipoStatusConfiavel  TipoStatus = "confiavel"
	TipoStatusAConfirmar TipoStatus = "a_confirmar"
)

// Status is the providência's own lifecycle, independent of TipoStatus: a providência is
// born SUGGESTED and stays there whether its tipo is confiável or not (SUGGESTED just means
// "not yet discarded"); DISCARDED is the lawyer's dismissal. CONFIRMED is reserved for a
// future fatia (the point at which a task is actually bound) — this slice never writes it.
type Status string

const (
	StatusSuggested Status = "SUGGESTED"
	StatusConfirmed Status = "CONFIRMED"
	StatusDiscarded Status = "DISCARDED"
)

// ActionItem is the Providência aggregate: the diagnostic that decides WHAT to do about
// one intimação (§1) and, when it generates a peça, WHICH tipo (§3). CourtRecordID,
// PieceProfileKey, DeadlineID and TaskID are all optional (empty string / nil), mirroring
// the nullable FKs of migration 0086. entity.go holds only the aggregate and its value
// types — it imports no repository/handler/lib (the slice's inward dependency rule).
type ActionItem struct {
	ID              string
	TenantID        string
	IntimationID    string
	CourtRecordID   string // "" when absent
	Tipo            string
	GeraPeca        bool
	PieceProfileKey string // "" when gera_peca is false
	TipoOrigem      TipoOrigem
	TipoStatus      TipoStatus
	DeadlineID      string // "" when the providência has no prazo bound yet
	Confianca       *float64
	Status          Status
	TaskID          string // "" until a future fatia's listener binds a task
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Tipo constants — the closed set the ERD names (§2): contestar | recorrer | manifestar |
// cumprir | ciencia. Kept as documentation/reuse anchors; validation.go's sanitizeTipo
// falls back to Ciencia for anything else the classifier proposes (viés seguro).
const (
	TipoContestar  = "contestar"
	TipoRecorrer   = "recorrer"
	TipoManifestar = "manifestar"
	TipoCumprir    = "cumprir"
	TipoCiencia    = "ciencia"
)

// knownPieceProfileKeys is the v1 catalog seeded by migration 0085 (docs/erd-tipos-de-
// peca.md §6). It is a local, hardcoded allowlist — NOT a query against piece_profile —
// because this slice never imports pieceprofile's package (slices talk by event/SQL-read,
// never by importing another slice's domain) and the FK already enforces the real
// constraint at insert time; this allowlist exists only so a hallucinated key from the
// classifier degrades a SINGLE candidate (gera_peca=false) instead of failing the whole
// acquisition.intimation.analyzed event with a DB FK violation. Extend it if the catalog
// grows — the FK is still the source of truth.
var knownPieceProfileKeys = map[string]bool{
	"peticao_inicial": true,
	"contestacao":     true,
	"apelacao":        true,
}
