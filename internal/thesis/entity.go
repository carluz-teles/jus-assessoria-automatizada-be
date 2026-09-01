package thesis

import "time"

// Thesis is the contract between the autos/teor and the peça (docs/erd-costura-
// providencia-tarefa-peca.md §4): a claim anchored in the process record and filtered
// by the notification's teor, tracked to the draft segments that honor it.
type Thesis struct {
	ID              string
	TenantID        string
	DraftID         string
	PieceProfileKey string
	NotificationID  string
	Enunciado       string
	Forca           string
	Estado          string
	Anchors         []ThesisAnchor
	CreatedAt       time.Time
}

// ThesisAnchor grounds a thesis in either a fact (from the autos) or a legal source.
type ThesisAnchor struct {
	ID            string
	ThesisID      string
	Tipo          string
	AlvoDocumento string
	AlvoFonte     string
	Motivo        string
	Status        string
}

// DraftSegment is one piece of drafted text that a thesis expanded into.
type DraftSegment struct {
	ID               string
	TenantID         string
	DraftID          string
	ThesisID         string
	ProfileSectionID string
	Conteudo         string
	Anchors          []SegmentAnchor
	CreatedAt        time.Time
}

// SegmentAnchor tracks whether a segment preserved the anchor its thesis carried.
type SegmentAnchor struct {
	ID             string
	DraftSegmentID string
	ThesisAnchorID string
	Status         string
}

// ThesisCoverage is the 1:0..1 verdict of whether an approved thesis was honored by
// the generated peça (docs/erd-costura-providencia-tarefa-peca.md §4.2).
type ThesisCoverage struct {
	ID        string
	ThesisID  string
	Resultado string
	Detalhe   string
	CreatedAt time.Time
}

type CoverageSummary struct {
	Coberta    int `json:"coberta"`
	Divergente int `json:"divergente"`
	Ausente    int `json:"ausente"`
}

const (
	ForcaFavoravel          = "favoravel"
	ForcaContrariaRelevante = "contraria_relevante"

	EstadoProposta   = "proposta"
	EstadoAprovada   = "aprovada"
	EstadoDescartada = "descartada"

	AnchorTipoFato    = "fato"
	AnchorTipoDireito = "direito"

	AnchorStatusAConfirmar = "a_confirmar"
	AnchorStatusValidada   = "validada"

	CoverageCoberta    = "coberta"
	CoverageDivergente = "divergente"
	CoverageAusente    = "ausente"
)
