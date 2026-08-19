package acquisition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// datajud_parser.go turns a DATAJUD RawPayload (the raw ElasticSearch envelope the
// connector reads) into a ParsedResult: the GRADED court record (DATAJUD discloses
// the grau DJEN never does, plus classe/assunto/órgão/ajuizamento/sigilo) and its
// movimentos as docket entries. It carries no intimations (DATAJUD has none) and no
// parties (LGPD — the public API omits them). Empty hits (the process is not yet in
// the index) yield an empty result, which the enrichment use case treats as a no-op.

// datajudDateLayout is the ISO date-time DATAJUD returns in dataAjuizamento
// (YYYYMMDDHHMMSS, e.g. "20161125000000").
const datajudDateLayout = "20060102150405"

// datajudCompleteness is the confidence a DATAJUD-sourced court record is fully
// populated: it is the authoritative source for the court's own view (grau, classe,
// órgão, movimentos), so it is high.
const datajudCompleteness = 0.9

// datajudFidelity is the docket-entry fidelity DATAJUD movimentos carry: they are
// the tribunal's own structured movements, the highest-fidelity andamento source.
const datajudFidelity = 100

// DATAJUDParser maps DATAJUD payloads. now stamps each docket entry's observed_at
// (when we learned of it); it defaults to time.Now and same-package tests override it.
type DATAJUDParser struct {
	now func() time.Time
}

// NewDATAJUDParser builds the parser.
func NewDATAJUDParser() *DATAJUDParser {
	return &DATAJUDParser{now: time.Now}
}

var _ Parser = (*DATAJUDParser)(nil)

// datajudResponse is the subset of the ES _search envelope the parser reads.
type datajudResponse struct {
	Hits struct {
		Hits []struct {
			Source datajudSource `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

// datajudSource is the subset of a process document the parser maps.
type datajudSource struct {
	NumeroProcesso  string             `json:"numeroProcesso"`
	Tribunal        string             `json:"tribunal"`
	Grau            string             `json:"grau"`
	DataAjuizamento string             `json:"dataAjuizamento"`
	NivelSigilo     int                `json:"nivelSigilo"`
	Classe          datajudCodeName    `json:"classe"`
	OrgaoJulgador   datajudOrgao       `json:"orgaoJulgador"`
	Assuntos        []datajudCodeName  `json:"assuntos"`
	Movimentos      []datajudMovimento `json:"movimentos"`
}

type datajudCodeName struct {
	Codigo int    `json:"codigo"`
	Nome   string `json:"nome"`
}

// datajudOrgao reads only the órgão name — its codigo is an int in _source but a
// string inside movimentos, so binding just the name avoids the type clash.
type datajudOrgao struct {
	Nome string `json:"nome"`
}

type datajudMovimento struct {
	Codigo                int             `json:"codigo"`
	Nome                  string          `json:"nome"`
	DataHora              string          `json:"dataHora"`
	ComplementosTabelados json.RawMessage `json:"complementosTabelados"`
}

// terminalMovementCodes is the CONSERVATIVE set of CNJ TPU movement codes that
// signal a definitive case closure (Baixa Definitiva). When the most-recent
// terminal movement in a DATAJUD payload belongs to this set, the court record is
// marked ARCHIVED. The set is intentionally MINIMAL to avoid false positives —
// marking an active process as archived HIDES it from the user, which is worse than
// leaving a closed case as ACTIVE. Extend this map only after validating the code's
// semantics against the CNJ TPU (Tabela de Movimentos Processuais) and confirming
// it always signals an irrecoverable end state. Unknown codes fall through silently
// to ACTIVE (the safe default).
//
// Validated against real DJEN/DATAJUD data (QA sweep). Extend only after confirming a
// code's semantics in the CNJ TPU (Tabela de Movimentos Processuais).
//
// Validated terminal codes:
//   - 22:  Baixa Definitiva (definitive removal from the court's docket)
//   - 196: Extinção da execução ou do cumprimento da sentença
var terminalMovementCodes = map[int]bool{
	22:  true, // Baixa Definitiva
	196: true, // Extinção da execução ou do cumprimento da sentença
}

// suspensionMovementCodes signal a process suspension (Suspensão/Sobrestamento).
//
// Validated suspension codes:
//   - 25:    Suspensão/Sobrestamento (parent code)
//   - 12065: Cumprimento de Suspensão ou Sobrestamento
var suspensionMovementCodes = map[int]bool{
	25:    true, // Suspensão/Sobrestamento
	12065: true, // Cumprimento de Suspensão ou Sobrestamento
}

// reactivationMovementCodes REOPEN a case after a prior close/suspension. When the
// most-recent STATE signal is a reactivation, the process is ACTIVE regardless of an
// earlier terminal/suspension movement — this is what makes the derivation robust to
// a baixa-then-desarquivamento (or suspensão-then-levantamento) history.
//
// Validated reactivation codes:
//   - 893:   Desarquivamento
//   - 12066: Cumprimento de Levantamento da Suspensão
var reactivationMovementCodes = map[int]bool{
	893:   true, // Desarquivamento
	12066: true, // Cumprimento de Levantamento da Suspensão
}

// lifecycleFromMovimentos derives the court_record lifecycle from a DATAJUD payload's
// movimento list, conservative-first: when in doubt, prefer ACTIVE (a false-negative
// is invisible; a false-positive HIDES the process from the user).
//
// Terminal is STICKY, suspension is FRAGILE — they resume differently:
//  1. Exclude movimentos with unparseable dataHora; sort the rest DESC (newest first).
//  2. ARCHIVED (sticky): walk newest→oldest — if a terminal code (baixa/extinção) is
//     reached BEFORE any reactivation code, the case is archived. A reactivation code
//     (desarquivamento/levantamento) more recent than the terminal short-circuits: the
//     case was reopened → not archived. Procedural paperwork does NOT un-archive.
//  3. SUSPENDED (fragile): only when the ABSOLUTE most-recent movement is a suspension
//     code — any later movement (even procedural) means the process resumed → ACTIVE.
//  4. Otherwise ACTIVE.
func lifecycleFromMovimentos(movs []datajudMovimento) string {
	type timed struct {
		codigo int
		ts     time.Time
	}
	valid := make([]timed, 0, len(movs))
	for _, m := range movs {
		ts, err := time.Parse(time.RFC3339, m.DataHora)
		if err != nil {
			continue
		}
		valid = append(valid, timed{codigo: m.Codigo, ts: ts})
	}
	if len(valid) == 0 {
		return LifecycleActive
	}

	// Insertion-sort descending by timestamp (slices are small; avoids importing sort).
	for i := 1; i < len(valid); i++ {
		for j := i; j > 0 && valid[j].ts.After(valid[j-1].ts); j-- {
			valid[j], valid[j-1] = valid[j-1], valid[j]
		}
	}

	// Terminal is sticky: newest→oldest, a reactivation short-circuits before a terminal.
	for _, m := range valid {
		if reactivationMovementCodes[m.codigo] {
			break // reopened after (or with no) terminal → not archived
		}
		if terminalMovementCodes[m.codigo] {
			return LifecycleArchived
		}
	}

	// Suspension is fragile: only if the absolute most-recent movement suspends.
	if suspensionMovementCodes[valid[0].codigo] {
		return LifecycleSuspended
	}

	return LifecycleActive
}

// CanParse claims any DATAJUD payload (matched by source).
func (*DATAJUDParser) CanParse(p RawPayload) bool { return p.Source == SourceDATAJUD }

// Parse maps the first hit (a numeroProcesso search returns at most one process per
// tribunal index) to the graded court record and its movimentos. No hit → empty
// result. A movimento with an unparseable dataHora is logged and skipped, not fatal.
func (p *DATAJUDParser) Parse(ctx context.Context, raw RawPayload) (ParsedResult, error) {
	var resp datajudResponse
	if err := json.Unmarshal(raw.Body, &resp); err != nil {
		return ParsedResult{}, fmt.Errorf("datajud: decode payload: %w", err)
	}
	if len(resp.Hits.Hits) == 0 {
		// The process is not (yet) in the tribunal index — nothing to enrich.
		return ParsedResult{}, nil
	}

	src := resp.Hits.Hits[0].Source
	degree := src.Grau
	if degree == "" {
		degree = DegreeUnknown
	}

	record := ParsedCourtRecord{
		CNJNumber:    src.NumeroProcesso,
		Degree:       degree,
		Court:        src.Tribunal,
		Class:        src.Classe.Nome,
		Subject:      firstAssunto(src.Assuntos),
		JudgingBody:  src.OrgaoJulgador.Nome,
		Completeness: datajudCompleteness,
		FiledAt:      parseDatajudDate(src.DataAjuizamento),
		Secrecy:      secrecyFromNivel(src.NivelSigilo),
		Lifecycle:    lifecycleFromMovimentos(src.Movimentos),
	}

	entries := make([]ParsedDocketEntry, 0, len(src.Movimentos))
	for _, mov := range src.Movimentos {
		occurred, err := time.Parse(time.RFC3339, mov.DataHora)
		if err != nil {
			slog.WarnContext(ctx, "datajud: skipping movimento with unparseable dataHora",
				"numero_processo", src.NumeroProcesso, "codigo", mov.Codigo, "data_hora", mov.DataHora)
			continue
		}
		entries = append(entries, ParsedDocketEntry{
			CNJNumber:   src.NumeroProcesso,
			Degree:      degree,
			Hash:        datajudMovimentoHash(src.Tribunal, src.NumeroProcesso, mov),
			OccurredAt:  occurred,
			ObservedAt:  p.now(),
			Source:      SourceDATAJUD,
			Fidelity:    datajudFidelity,
			Text:        mov.Nome,
			TPUCode:     mov.Codigo,
			Complements: mov.ComplementosTabelados,
		})
	}

	return ParsedResult{
		CourtRecords:  []ParsedCourtRecord{record},
		DocketEntries: entries,
	}, nil
}

// datajudMovimentoHash derives the docket-entry dedup key: DATAJUD gives no hash of
// its own, so it is sha256(tribunal|number|dataHora|codigo|nome) — stable, so a
// re-fetch of the same process dedups its movimentos. The grau is deliberately OUT of
// the key: with FIX B there is one court_record per CNJ (the placeholder is graded in
// place, not duplicated), and docket_entry's UNIQUE (court_record_id, hash) already
// separates records — so hashing the grau would only re-duplicate a movimento the
// moment DATAJUD reveals or changes the grade.
func datajudMovimentoHash(tribunal, numero string, mov datajudMovimento) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		tribunal, numero, mov.DataHora, strconv.Itoa(mov.Codigo), mov.Nome,
	}, "|")))
	return hex.EncodeToString(sum[:])
}

// firstAssunto returns the primary subject name (v0 keeps a single subject), or ""
// when the process lists none.
func firstAssunto(assuntos []datajudCodeName) string {
	if len(assuntos) == 0 {
		return ""
	}
	return assuntos[0].Nome
}

// secrecyFromNivel maps DATAJUD's nivelSigilo (0 public, higher = more restricted)
// onto the schema's secrecy enum.
func secrecyFromNivel(nivel int) string {
	switch {
	case nivel <= 0:
		return SecrecyPublic
	case nivel >= 5:
		return SecrecySecret
	default:
		return SecrecyRestricted
	}
}

// parseDatajudDate parses dataAjuizamento (YYYYMMDDHHMMSS), returning the zero time
// when it is absent or malformed.
func parseDatajudDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(datajudDateLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
