package acquisition

import "strings"

// Fase is the procedural phase a process is in — the cockpit's stepper
// (Conhecimento → Instrução → Sentença → Recurso → Execução). It is DERIVED, not a
// field any source carries: no tribunal exposes a "fase", but the CNJ movimentos
// (docket_entry.tpu_code, the national TPU taxonomy) and the classe together pin it
// down. The user can still override it by hand in the cockpit — the derivation is the
// default, not the last word.
type Fase = string

const (
	FaseConhecimento Fase = "CONHECIMENTO"
	FaseInstrucao    Fase = "INSTRUCAO"
	FaseSentenca     Fase = "SENTENCA"
	FaseRecurso      Fase = "RECURSO"
	FaseExecucao     Fase = "EXECUCAO"
)

// faseOrder ranks the phases so the derivation can take the FURTHEST one reached: a
// process at cumprimento (Execução) has necessarily passed Sentença, so the stepper
// marks the highest ordinal any movimento (or the classe baseline) reached.
var faseOrder = map[Fase]int{
	FaseConhecimento: 1,
	FaseInstrucao:    2,
	FaseSentenca:     3,
	FaseRecurso:      4,
	FaseExecucao:     5,
}

var faseByOrder = map[int]Fase{
	1: FaseConhecimento,
	2: FaseInstrucao,
	3: FaseSentenca,
	4: FaseRecurso,
	5: FaseExecucao,
}

// faseByTPUCode maps the phase-defining CNJ movimento codes (TPU) to the phase they
// mark. Only the codes that MOVE the phase are here; the vast majority of movimentos
// (Expedição de documento, Ato ordinatório, Publicação, Remessa…) are neutral and
// absent on purpose. Curated from the codes actually observed in our docket_entry
// data — see the code→descrição catalog in the cockpit-data investigation.
var faseByTPUCode = map[int]Fase{
	// ② Instrução — saneamento, audiências, tutela/liminar interlocutória
	12387: FaseInstrucao, // Decisão de Saneamento e Organização
	970:   FaseInstrucao, // Audiência
	12749: FaseInstrucao, // Audiência de Instrução
	12750: FaseInstrucao, // Audiência de Instrução e Julgamento
	12751: FaseInstrucao, // Audiência de Julgamento
	12624: FaseInstrucao, // Audiência do art. 334 CPC (conciliação)
	12740: FaseInstrucao, // Audiência de Conciliação
	12753: FaseInstrucao, // Preliminar
	792:   FaseInstrucao, // Liminar
	339:   FaseInstrucao, // Liminar
	892:   FaseInstrucao, // Liminar
	785:   FaseInstrucao, // Antecipação de tutela
	332:   FaseInstrucao, // Antecipação de tutela
	889:   FaseInstrucao, // Antecipação de Tutela
	347:   FaseInstrucao, // Antecipação de Tutela
	// ③ Sentença — mérito julgado / homologado + embargos de declaração pós-sentença
	193:   FaseSentenca, // Julgamento
	219:   FaseSentenca, // Procedência
	220:   FaseSentenca, // Improcedência
	221:   FaseSentenca, // Procedência em Parte
	11408: FaseSentenca, // Improcedência do pedido e procedência em parte do contraposto
	466:   FaseSentenca, // Homologação de Transação
	12185: FaseSentenca, // Decisão Interlocutória de Mérito
	12187: FaseSentenca, // Homologação de Decisão de Juiz Leigo
	198:   FaseSentenca, // Acolhimento de Embargos de Declaração
	200:   FaseSentenca, // Não-Acolhimento de Embargos de Declaração
	871:   FaseSentenca, // Acolhimento em parte de Embargos de Declaração
	15162: FaseSentenca, // Acolhimento de Embargos de Declaração
	15163: FaseSentenca, // Acolhimento em Parte de Embargos de Declaração
	15164: FaseSentenca, // Não Acolhimento de Embargos de Declaração
	// ④ Recurso
	804:   FaseRecurso, // Recurso
	381:   FaseRecurso, // Recurso
	1060:  FaseRecurso, // Recurso
	1059:  FaseRecurso, // Recurso (sem efeito suspensivo)
	429:   FaseRecurso, // Recurso extraordinário
	430:   FaseRecurso, // Recurso especial
	432:   FaseRecurso, // Recurso Extraordinário
	433:   FaseRecurso, // Recurso Especial
	434:   FaseRecurso, // Recurso de Revista
	265:   FaseRecurso, // Recurso Extraordinário com repercussão geral
	11975: FaseRecurso, // Recurso Especial repetitivo
	235:   FaseRecurso, // Não Conhecimento de recurso
	230:   FaseRecurso, // Recurso prejudicado
	14975: FaseRecurso, // Suspensão por RE com Repercussão Geral
	14976: FaseRecurso, // Suspensão por REsp Repetitivo
	// ⑤ Execução / Cumprimento
	11385: FaseExecucao, // Execução/Cumprimento de Sentença Iniciada
	848:   FaseExecucao, // Trânsito em julgado
	196:   FaseExecucao, // Extinção da execução ou do cumprimento da sentença
	276:   FaseExecucao, // Execução frustrada
	383:   FaseExecucao, // Impugnação ao cumprimento de sentença
	14099: FaseExecucao, // Homologação de Acordo em Execução ou Cumprimento
	277:   FaseExecucao, // Convenção das Partes p/ Satisfação em Execução
	788:   FaseExecucao, // Exceção de pré-executividade
	335:   FaseExecucao, // Exceção de pré-executividade
}

// faseFromClasse is the baseline phase read off the process class alone: an execução
// or cumprimento class starts already in Execução; everything else starts in
// Conhecimento and is refined upward by the movimentos.
func faseFromClasse(classe string) Fase {
	c := strings.ToLower(classe)
	if strings.Contains(c, "execu") || strings.Contains(c, "cumprimento de senten") {
		return FaseExecucao
	}
	return FaseConhecimento
}

// faseFromClassAndMovimentos derives the process phase: the FURTHEST phase reached by
// the classe baseline or any phase-defining movimento. Mirrors lifecycleFromMovimentos'
// posture — pure, tolerant of unparsable dates (a movimento with a bad DataHora still
// counts for its code; the phase is order-based, not recency-based). Neutral movimentos
// (not in faseByTPUCode) never move the phase.
func faseFromClassAndMovimentos(classe string, movs []datajudMovimento) Fase {
	best := faseOrder[faseFromClasse(classe)]
	for _, m := range movs {
		if f, ok := faseByTPUCode[m.Codigo]; ok {
			if o := faseOrder[f]; o > best {
				best = o
			}
		}
	}
	return faseByOrder[best]
}
