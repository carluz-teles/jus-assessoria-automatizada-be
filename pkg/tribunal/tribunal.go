// Package tribunal is the reference registry of Brazilian courts (tribunais) whose
// publications appear in the DJEN national daily. It is pure reference DATA — no
// business rule — so it lives in /pkg, shared by more than one slice without either
// importing the other:
//
//   - acquisition sweeps Tribunais in the national ingestion and denormalizes UF from
//     a court sigla at emission (UF);
//   - deadline derives UF from court_record.court for the state-holiday calendar
//     lookup (UF), without importing acquisition (slices talk by event, decisão P1).
//
// This registry is the single source that (a) enumerates the courts the national
// ingestion sweeps and (b) backs UF.
package tribunal

import (
	"strconv"
	"strings"
)

// Tribunal is one court whose publications appear in the DJEN national daily — the
// value the Comunica API accepts as its siglaTribunal filter. UF is the two-letter
// state when the court is state-scoped (TJs, TREs and the state military courts),
// used for the state-holiday deadline lookup; it is "" for region- or nationally
// scoped courts (TRFs, TRTs, superior courts), where only national and court-scoped
// holidays apply.
type Tribunal struct {
	Sigla string
	UF    string
}

// ufsBR are the 26 states + the Federal District — the scope of the state (TJ) and
// electoral (TRE) courts.
var ufsBR = []string{
	"AC", "AL", "AP", "AM", "BA", "CE", "DF", "ES", "GO", "MA", "MT", "MS", "MG",
	"PA", "PB", "PR", "PE", "PI", "RJ", "RN", "RO", "RR", "RS", "SC", "SE", "SP", "TO",
}

// Tribunais is the canonical list of DJEN courts to sweep in the national daily
// ingestion. The set and the exact sigla spellings were validated against the live
// Comunica API (2026-08): state courts are "TJ"+UF except the Federal District,
// grafado TJDFT (TJDF 500s); electoral courts are hyphenated "TRE-"+UF (TRESP 500s,
// TRE-SP is accepted); STF is excluded (it 500s on this endpoint). A sigla that ever
// returns empty/error is harmlessly skipped by the ingestion, so extending this list
// is low-risk.
var Tribunais = buildTribunais()

// tribunalUF indexes Tribunais by sigla (already upper-cased) for UF.
var tribunalUF = indexTribunalUF(Tribunais)

func buildTribunais() []Tribunal {
	ts := make([]Tribunal, 0, 91)

	// Estadual (justiça comum dos estados): TJ+UF, com o DF grafado TJDFT.
	for _, uf := range ufsBR {
		sigla := "TJ" + uf
		if uf == "DF" {
			sigla = "TJDFT"
		}
		ts = append(ts, Tribunal{Sigla: sigla, UF: uf})
	}

	// Militar estadual: só SP, MG e RS mantêm tribunal de justiça militar próprio.
	for _, uf := range []string{"SP", "MG", "RS"} {
		ts = append(ts, Tribunal{Sigla: "TJM" + uf, UF: uf})
	}

	// Eleitoral: TRE-UF (hifenizado), um por estado + DF.
	for _, uf := range ufsBR {
		ts = append(ts, Tribunal{Sigla: "TRE-" + uf, UF: uf})
	}

	// Federal: 6 regiões (a 6ª criada em 2022); região abrange vários estados, sem UF única.
	for i := 1; i <= 6; i++ {
		ts = append(ts, Tribunal{Sigla: "TRF" + strconv.Itoa(i)})
	}

	// Trabalho: 24 regiões, sem UF única.
	for i := 1; i <= 24; i++ {
		ts = append(ts, Tribunal{Sigla: "TRT" + strconv.Itoa(i)})
	}

	// Superiores nacionais (STF fora — 500 no endpoint).
	for _, s := range []string{"STJ", "STM", "TST", "TSE"} {
		ts = append(ts, Tribunal{Sigla: s})
	}

	return ts
}

func indexTribunalUF(ts []Tribunal) map[string]string {
	m := make(map[string]string, len(ts))
	for _, t := range ts {
		m[t.Sigla] = t.UF
	}
	return m
}

// UF resolves the state UF of a tribunal sigla for the state-holiday lookup, from the
// registry (single source): state, electoral and state military courts (TJSP → SP,
// TRE-SP → SP, TJMSP → SP) carry their UF, while region- and nationally-scoped courts
// (TRF/TRT/STJ/…) and any unknown sigla resolve to "" so the calendar applies only
// national + court-scoped holidays.
func UF(sigla string) string {
	return tribunalUF[strings.ToUpper(strings.TrimSpace(sigla))]
}
