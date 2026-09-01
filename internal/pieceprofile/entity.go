package pieceprofile

import "time"

// BaseSkeleton is the invariant frame (endereçamento, preâmbulo, ⟦miolo⟧, pedidos,
// fecho) shared by every piece_profile — docs/erd-tipos-de-peca.md §1/§2.
type BaseSkeleton struct {
	Key   string
	Slots []byte
}

// Matter is the transversal axis (cível/trabalhista/penal) a piece_profile is
// indexed by — docs/erd-tipos-de-peca.md §2.
type Matter struct {
	Key  string
	Nome string
}

// FormatProfile is the appearance default (font, spacing, margins) applied at
// export time — separate from content (docs/erd-tipos-de-peca.md §2).
type FormatProfile struct {
	Key                 string
	Fonte               string
	TamanhoCorpo        int
	TamanhoCitacaoLonga int
	Espacamento         string
	Alinhamento         string
	Margens             []byte
	CitacaoLonga        []byte
	Export              string
}

// PieceProfile is the catalog row for one tipo de peça (contestação, apelação,
// ...). VersionAtual is a free-form label ("v1", "v1.1", "2025-09-01", ...), not
// a counter: a new PieceProfileVersion always carries an EXPLICIT version chosen
// by the caller, never one derived by incrementing this field.
type PieceProfile struct {
	Key              string
	Nome             string
	Polo             string
	MatterKey        string
	BaseSkeletonKey  string
	FormatProfileKey string
	VersionAtual     string
	FonteLegal       []byte
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Sections         []ProfileSection
	Requirements     []ProfileRequirement
}

// ProfileSection is one ordered miolo section of a piece_profile.
type ProfileSection struct {
	ID              string
	PieceProfileKey string
	Key             string
	Titulo          string
	Ordem           int
	Obrigatoria     string
	Origem          string
	AceitaTeses     bool
	FonteLegal      []byte
}

// ProfileRequirement is a field the piece_profile demands (e.g. valor_causa).
type ProfileRequirement struct {
	ID              string
	PieceProfileKey string
	Campo           string
	Obrigatorio     bool
	FonteLegal      []byte
}

// PieceProfileVersion is a frozen snapshot of a piece_profile's sections + rules
// at a point in time (docs/erd-tipos-de-peca.md §2). Version is the caller-chosen
// label, never inferred.
type PieceProfileVersion struct {
	ID              string
	PieceProfileKey string
	Version         string
	VigenteDesde    time.Time
	Snapshot        []byte
}

const (
	PoloAtivo   = "ativo"
	PoloPassivo = "passivo"
	PoloAmbos   = "ambos"
)

var validPolos = map[string]bool{
	PoloAtivo:   true,
	PoloPassivo: true,
	PoloAmbos:   true,
}

const (
	ObrigatoriaSim         = "sim"
	ObrigatoriaNao         = "nao"
	ObrigatoriaCondicional = "condicional"
)

var validObrigatorias = map[string]bool{
	ObrigatoriaSim:         true,
	ObrigatoriaNao:         true,
	ObrigatoriaCondicional: true,
}

const (
	OrigemMoldura       = "moldura"
	OrigemArgumentativa = "argumentativa"
)

var validOrigens = map[string]bool{
	OrigemMoldura:       true,
	OrigemArgumentativa: true,
}
