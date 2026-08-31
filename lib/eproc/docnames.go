package eproc

import "strings"

// documentTypeLabels maps an eproc "tipo de documento" code (the data-nome
// attribute on a.infraLinkDocumento) to a friendly pt-BR label. eproc labels
// documents with terse uppercase codes (PET, SENT, DESPADEC, CONTRSOCIAL…);
// this table turns them into the names a lawyer expects to read in the autos.
// It is deliberately partial: an UNKNOWN code returns "" from DocumentTypeLabel
// so the caller can fall back to the richer event description (infraEventoDescricao)
// before, as a last resort, showing the raw code.
var documentTypeLabels = map[string]string{
	// petições / manifestações / recursos
	"PET":        "Petição",
	"PETINI":     "Petição inicial",
	"INIC":       "Petição inicial",
	"CONT":       "Contestação",
	"REPL":       "Réplica",
	"MANIF":      "Manifestação",
	"EMBDECL":    "Embargos de declaração",
	"RECURSO":    "Recurso",
	"APEL":       "Apelação",
	"AGRAVO":     "Agravo",
	"CONTRAZ":    "Contrarrazões",
	// decisões / despachos / sentenças
	"SENT":     "Sentença",
	"DEC":      "Decisão",
	"DECISAO":  "Decisão",
	"DESP":     "Despacho",
	"DESPADEC": "Despacho / Decisão",
	"ATOORD":   "Ato ordinatório",
	// certidões / comunicações / mandados
	"CERT":            "Certidão",
	"CARTA":           "Carta",
	"AR":              "Aviso de recebimento",
	"OFIC":            "Ofício",
	"MAND":            "Mandado",
	"INTIM":           "Intimação",
	"PRECATORIA":      "Carta precatória",
	"PROTOCOLO ORDEM": "Protocolo",
	// provas / anexos / instrução
	"LAUDO":             "Laudo pericial",
	"DOC":               "Documento",
	"DOCUMENTACAO":      "Documentação",
	"PROC":              "Procuração",
	"CONTR":             "Contrato",
	"CONTRSOCIAL":       "Contrato social",
	"CALC":              "Cálculo",
	"COMP":              "Comprovante",
	"GUIA":              "Guia",
	"CDA":               "Certidão de dívida ativa",
	"ATA":               "Ata",
	"EMAIL":             "E-mail",
	"REL.PESQ.ENDERECO": "Pesquisa de endereço",
	"DETSISPARTOT":      "Detalhamento",
	"OUT":               "Outros",
}

// DocumentTypeLabel returns the friendly pt-BR label for an eproc document type
// code, or "" when the code is unknown (so the caller can fall back to the event
// description). The lookup is case-insensitive and trims surrounding whitespace.
func DocumentTypeLabel(code string) string {
	return documentTypeLabels[strings.ToUpper(strings.TrimSpace(code))]
}
