package acquisition

// title.go — the read-model title composition (Achado 1: today's title is
// "classe · assunto", generic and repeated across 62-74% of processes since the
// classe/assunto pair rarely differentiates cases in the same escritório). Pure,
// no banco — the read side (repository.go's ListProcessos/GetProcesso/ListIntimacoes/
// GetIntimacao mappers) feeds it the label/réu already fetched by the query's own
// JOINs (court_case.label, the first-captured DEFENDANT party). No new table, no
// cross-slice event — this is a projection over data the acquisition slice already owns.

// BuildCaseTitle composes the process title shown to the advogado (lista de processos,
// painel/detalhe de intimação, cockpit), replacing the old always-classe·assunto title.
// Priority, highest first:
//  1. label — the advogado's own manual título (court_case.label via PATCH
//     /v1/processos/:id), when set;
//  2. defendantName + " · " + cnjNumber — the first captured réu, when a party exists but
//     no manual label was set yet;
//  3. class + " · " + subject — the original fallback, UNCHANGED (zero regression when
//     neither a label nor a captured réu exists yet).
//
// cnjNumber is NEVER folded into the title in branches (1) or (3) — it stays a separate
// subtitle field in the DTO (ProcessoView.CNJNumber / IntimacaoView.CNJNumber). It is
// concatenated ONLY inside branch (2), per the locked architecture decision.
//
// An empty label or an empty defendantName (defensive — the write/read paths already
// normalize "" to nil/NULL so this should not occur in practice) falls through to the
// next priority rather than producing a blank or malformed title.
func BuildCaseTitle(label *string, defendantName *string, cnjNumber, class, subject string) string {
	if label != nil && *label != "" {
		return *label
	}
	if defendantName != nil && *defendantName != "" {
		return *defendantName + " · " + cnjNumber
	}
	return class + " · " + subject
}
