package acquisition

import "strconv"

// tribunal is one court whose publications appear in the DJEN national daily — the
// value the Comunica API accepts as its siglaTribunal filter. uf is the two-letter
// state when the court is state-scoped (TJs, TREs and the state military courts),
// used for the state-holiday deadline lookup; it is "" for region- or nationally
// scoped courts (TRFs, TRTs, superior courts), where only national and court-scoped
// holidays apply. This registry is the single source that (a) enumerates the courts
// the national ingestion sweeps and (b) backs ufFromTribunal.
type tribunal struct {
	sigla string
	uf    string
}

// ufsBR are the 26 states + the Federal District — the scope of the state (TJ) and
// electoral (TRE) courts.
var ufsBR = []string{
	"AC", "AL", "AP", "AM", "BA", "CE", "DF", "ES", "GO", "MA", "MT", "MS", "MG",
	"PA", "PB", "PR", "PE", "PI", "RJ", "RN", "RO", "RR", "RS", "SC", "SE", "SP", "TO",
}

// tribunais is the canonical list of DJEN courts to sweep in the national daily
// ingestion. The set and the exact sigla spellings were validated against the live
// Comunica API (2026-08): state courts are "TJ"+UF except the Federal District,
// grafado TJDFT (TJDF 500s); electoral courts are hyphenated "TRE-"+UF (TRESP 500s,
// TRE-SP is accepted); STF is excluded (it 500s on this endpoint). A sigla that ever
// returns empty/error is harmlessly skipped by the ingestion, so extending this list
// is low-risk.
var tribunais = buildTribunais()

// tribunalUF indexes tribunais by sigla (already upper-cased) for ufFromTribunal.
var tribunalUF = indexTribunalUF(tribunais)

func buildTribunais() []tribunal {
	ts := make([]tribunal, 0, 91)

	// Estadual (justiça comum dos estados): TJ+UF, com o DF grafado TJDFT.
	for _, uf := range ufsBR {
		sigla := "TJ" + uf
		if uf == "DF" {
			sigla = "TJDFT"
		}
		ts = append(ts, tribunal{sigla: sigla, uf: uf})
	}

	// Militar estadual: só SP, MG e RS mantêm tribunal de justiça militar próprio.
	for _, uf := range []string{"SP", "MG", "RS"} {
		ts = append(ts, tribunal{sigla: "TJM" + uf, uf: uf})
	}

	// Eleitoral: TRE-UF (hifenizado), um por estado + DF.
	for _, uf := range ufsBR {
		ts = append(ts, tribunal{sigla: "TRE-" + uf, uf: uf})
	}

	// Federal: 6 regiões (a 6ª criada em 2022); região abrange vários estados, sem UF única.
	for i := 1; i <= 6; i++ {
		ts = append(ts, tribunal{sigla: "TRF" + strconv.Itoa(i)})
	}

	// Trabalho: 24 regiões, sem UF única.
	for i := 1; i <= 24; i++ {
		ts = append(ts, tribunal{sigla: "TRT" + strconv.Itoa(i)})
	}

	// Superiores nacionais (STF fora — 500 no endpoint).
	for _, s := range []string{"STJ", "STM", "TST", "TSE"} {
		ts = append(ts, tribunal{sigla: s})
	}

	return ts
}

func indexTribunalUF(ts []tribunal) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[t.sigla] = t.uf
	}
	return m
}
