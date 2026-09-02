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
	"EMBEXEC":    "Embargos à execução",
	"RECURSO":    "Recurso",
	"APEL":       "Apelação",
	"AGRAVO":     "Agravo",
	"AGRINST":    "Agravo de instrumento",
	"CONTRAZ":    "Contrarrazões",
	"RAZOES":     "Razões",
	"IMPUG":      "Impugnação",
	"EXCECAO":    "Exceção",
	"RECLAM":     "Reclamação",
	// decisões / despachos / sentenças
	"SENT":     "Sentença",
	"DEC":      "Decisão",
	"DECISAO":  "Decisão",
	"DESP":     "Despacho",
	"DESPADEC": "Despacho / Decisão",
	"ATOORD":   "Ato ordinatório",
	"ACORDAO":  "Acórdão",
	"VOTO":     "Voto",
	// certidões / comunicações / mandados
	"CERT":            "Certidão",
	"CARTA":           "Carta",
	"AR":              "Aviso de recebimento",
	"OFIC":            "Ofício",
	"MAND":            "Mandado",
	"INTIM":           "Intimação",
	"CITACAO":         "Citação",
	"NOTIF":           "Notificação",
	"EDITAL":          "Edital",
	"ALVARA":          "Alvará",
	"PRECATORIA":      "Carta precatória",
	"ROGATORIA":       "Carta rogatória",
	"PROTOCOLO ORDEM": "Protocolo",
	// provas / anexos / instrução
	"LAUDO":              "Laudo pericial",
	"PARECER":            "Parecer",
	"DOC":                "Documento",
	"DOCUMENTACAO":       "Documentação",
	"PROC":               "Procuração",
	"PROCURACAO":         "Procuração",
	"SUBST":              "Substabelecimento",
	"SUBSTABELECIMENTO":  "Substabelecimento",
	"CONTR":              "Contrato",
	"CONTRSOCIAL":        "Contrato social",
	"CALC":               "Cálculo",
	"PLANILHA DE CÁLCULO": "Planilha de cálculo",
	"COMP":               "Comprovante",
	"COMPROVANTE":        "Comprovante",
	"GUIA":               "Guia",
	"CDA":                "Certidão de dívida ativa",
	"ATA":                "Ata",
	"ATAAUD":             "Ata de audiência",
	"TERMO":              "Termo",
	"EMAIL":              "E-mail",
	"RG":                 "Documento de identidade",
	"CPF":                "CPF",
	"CNH":                "Carteira de habilitação",
	"REL.PESQ.ENDERECO":  "Pesquisa de endereço",
	"DETSISPARTOT":       "Detalhamento",
	"OUT":                "Outros",
}

// DocumentTypeLabel returns the friendly pt-BR label for an eproc document type
// code, or "" when the code is unknown (so the caller can fall back to the event
// description). The lookup is case-insensitive and trims surrounding whitespace.
func DocumentTypeLabel(code string) string {
	return documentTypeLabels[strings.ToUpper(strings.TrimSpace(code))]
}

// HumanizeCode turns a RAW eproc code that has no entry in the label table into
// a readable pt-BR form: it lower-cases the whole string (preserving accents)
// and upper-cases only the first rune, so "PLANILHA DE CÁLCULO" reads
// "Planilha de cálculo" and "INCRESSIS" reads "Incressis". It is the last-resort
// fallback docTitleAndType uses before showing the untouched code — a code that
// is already a friendly label (or that we simply don't have a mapping for) still
// comes out human-shaped instead of SHOUTING. Empty in, empty out.
func HumanizeCode(code string) string {
	trimmed := strings.TrimSpace(code)
	if trimmed == "" {
		return ""
	}
	lowered := strings.ToLower(trimmed)
	r := []rune(lowered)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}
