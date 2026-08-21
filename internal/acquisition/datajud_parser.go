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

// datajudResponse is the subset of the ES _search envelope the parser reads. Each hit's
// _source is held as RawMessage and parsed INDIVIDUALLY (parseSource): DATAJUD occasionally
// ships a single malformed process among thousands (e.g. a nested-array `assuntos`), and a
// page-wide Unmarshal would fail the ENTIRE page on that one hit — killing a whole tribunal's
// enrichment chain and hanging the import. Per-hit parsing isolates the bad process (logged
// + skipped) so the sweep keeps progressing.
type datajudResponse struct {
	Hits struct {
		Hits []struct {
			Source json.RawMessage `json:"_source"`
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
	Assuntos        datajudAssuntos    `json:"assuntos"`
	Movimentos      []datajudMovimento `json:"movimentos"`
}

type datajudCodeName struct {
	Codigo int    `json:"codigo"`
	Nome   string `json:"nome"`
}

// datajudAssuntos is a lenient list of {codigo,nome}: DATAJUD normally sends a flat array of
// objects, but SOME processes carry a MIXED shape where an element is itself a NESTED ARRAY
// of objects (e.g. `[{...}, [{"codigo":9580,"nome":"..."}], ...]`). A plain []datajudCodeName
// fails to unmarshal that array-into-object. This UnmarshalJSON FLATTENS one level of nesting
// and IGNORES any element that is neither an object nor an array of objects — the parser only
// needs the first valid subject name, so a malformed element must never be fatal.
type datajudAssuntos []datajudCodeName

func (a *datajudAssuntos) UnmarshalJSON(data []byte) error {
	// Decode into raw elements first so a mixed shape (object vs. nested array) does not fail
	// the whole field; then classify each element leniently.
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// Not even an array (null/object/garbage): treat as no subjects rather than fatal.
		return nil
	}
	out := make(datajudAssuntos, 0, len(raw))
	for _, el := range raw {
		var obj datajudCodeName
		if err := json.Unmarshal(el, &obj); err == nil {
			out = append(out, obj)
			continue
		}
		// Not an object — try a nested array of objects and flatten one level.
		var nested []datajudCodeName
		if err := json.Unmarshal(el, &nested); err == nil {
			out = append(out, nested...)
			continue
		}
		// Neither an object nor an array of objects (e.g. a bare string/number): skip it.
	}
	*a = out
	return nil
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
// Validated codes:
//   - 22: Baixa Definitiva (definitive removal from the court's docket)
var terminalMovementCodes = map[int]bool{
	22: true, // Baixa Definitiva
}

// suspensionMovementCodes is the CONSERVATIVE set of CNJ TPU movement codes that
// signal a process suspension (Suspensão/Sobrestamento). A process is marked
// SUSPENDED only when the absolute most-recent movement belongs to this set (i.e.
// no later movement reverted it, which would mean the process resumed). Unknown
// codes fall through silently to ACTIVE (safe default).
//
// Validated codes:
//   - 25: Suspensão/Sobrestamento
var suspensionMovementCodes = map[int]bool{
	25: true, // Suspensão/Sobrestamento
}

// lifecycleFromMovimentos derives the court_record lifecycle from a DATAJUD payload's
// movimento list, conservative-first: when in doubt, prefer ACTIVE (a false-negative
// is invisible; a false-positive hides the process from the user).
//
// Algorithm:
//  1. Exclude movimentos with unparseable dataHora (same as the entry builder).
//  2. Sort remaining by dataHora descending (most recent first).
//  3. Walk the sorted list: if any movement's code is in terminalMovementCodes,
//     return ARCHIVED — a later procedural step does not un-close a definitively
//     closed case, so the terminal code wins regardless of position.
//  4. If the absolute most-recent movement's code is in suspensionMovementCodes,
//     return SUSPENDED — no later movement overrode it.
//  5. Otherwise return ACTIVE.
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

	// Terminal check: walk all movements; first terminal code encountered → ARCHIVED.
	for _, m := range valid {
		if terminalMovementCodes[m.codigo] {
			return LifecycleArchived
		}
	}

	// Suspension check: only if the absolute most-recent movement signals suspension.
	if suspensionMovementCodes[valid[0].codigo] {
		return LifecycleSuspended
	}

	return LifecycleActive
}

// CanParse claims any DATAJUD payload (matched by source).
func (*DATAJUDParser) CanParse(p RawPayload) bool { return p.Source == SourceDATAJUD }

// Parse maps the first hit (a numeroProcesso search returns at most one process per
// tribunal index) to the graded court record and its movimentos. No hit → empty
// result. The single hit is parsed with the SAME per-hit path as the batch, so a
// malformed _source (e.g. a nested-array `assuntos`) is a no-op result rather than a fatal
// error — a bad process must never fail the whole payload.
func (p *DATAJUDParser) Parse(ctx context.Context, raw RawPayload) (ParsedResult, error) {
	var resp datajudResponse
	if err := json.Unmarshal(raw.Body, &resp); err != nil {
		return ParsedResult{}, fmt.Errorf("datajud: decode payload: %w", err)
	}
	if len(resp.Hits.Hits) == 0 {
		// The process is not (yet) in the tribunal index — nothing to enrich.
		return ParsedResult{}, nil
	}

	proc, _, outcome := p.parseHit(ctx, resp.Hits.Hits[0].Source)
	if outcome != hitParsed {
		// Malformed or ungraded top hit: nothing to enrich (the by-number caller treats an
		// empty result as a no-op ack, same as a no-hit response).
		return ParsedResult{}, nil
	}
	return ParsedResult{
		CourtRecords:  []ParsedCourtRecord{proc.Record},
		DocketEntries: proc.Movimentos,
	}, nil
}

// parseHitOutcome classifies what became of ONE hit's _source, so the batch can tell a REAL
// error (DATAJUD returned a hit we could not parse) apart from a benign non-grade.
type parseHitOutcome int

const (
	// hitParsed: the _source parsed into a graded process.
	hitParsed parseHitOutcome = iota
	// hitSkippedNoGrade: the _source parsed but carries no grau — nothing to grade, NOT an
	// error (the number just is not enrichable yet).
	hitSkippedNoGrade
	// hitParseError: the _source itself failed to unmarshal (a hit present but not parseable)
	// — a REAL error, counted toward capture_run.errors so the fecho can be PARTIAL.
	hitParseError
)

// parseHit maps ONE hit's raw _source into a graded process, tolerantly. It returns the
// numeroProcesso (recovered even on a parse failure, for the log/error tally) and an outcome:
// a _source that fails to unmarshal is LOGGED and reported as hitParseError — never a fatal
// error — so one bad process among thousands is skipped instead of failing the whole page. A
// hit with an empty grau is hitSkippedNoGrade (benign, not an error).
func (p *DATAJUDParser) parseHit(ctx context.Context, rawSource json.RawMessage) (proc ParsedProcess, numero string, outcome parseHitOutcome) {
	var src datajudSource
	if err := json.Unmarshal(rawSource, &src); err != nil {
		// Try to recover the numeroProcesso for the log/tally even when the full parse failed.
		var idOnly struct {
			NumeroProcesso string `json:"numeroProcesso"`
		}
		_ = json.Unmarshal(rawSource, &idOnly)
		slog.WarnContext(ctx, "datajud: skipping unparseable _source",
			"numero_processo", idOnly.NumeroProcesso, "error", err.Error())
		return ParsedProcess{}, idOnly.NumeroProcesso, hitParseError
	}

	degree := src.Grau
	if degree == "" {
		// DATAJUD did not disclose a grade — nothing to grade in place (benign, not an error).
		return ParsedProcess{}, src.NumeroProcesso, hitSkippedNoGrade
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

	return ParsedProcess{Record: record, Movimentos: entries}, src.NumeroProcesso, hitParsed
}

// ParsedProcess is one process a batch enrichment resolved: the graded court record
// and its movimentos, keyed back to the numeroProcesso the caller asked for. The
// enrichment job reuses the same apply core as the single-fetch path, one process at
// a time.
type ParsedProcess struct {
	Record     ParsedCourtRecord
	Movimentos []ParsedDocketEntry
}

// ParseBatch maps a batch (`terms`) DATAJUD payload — potentially MANY hits, and
// potentially several pages — into a map keyed by numeroProcesso, plus the numeroProcessos
// that were SKIPPED BY A REAL PARSE ERROR (a hit present in the envelope that we could not
// unmarshal). It is PER-HIT resilient: each hit's _source is parsed individually (parseHit),
// so ONE malformed process (e.g. a broken _source) is logged + counted + skipped instead of
// failing the whole page — a bad process must never kill the tribunal's chain and hang the
// import. The `skipped` slice is what the fecho uses to decide OK vs PARTIAL: a number that
// was simply ABSENT from the envelope (0 hits — not in the DATAJUD index) is NOT in `skipped`
// (that is EXPECTED, not a failure); only a hit-present-but-unparseable is. A hit with an
// empty grau is dropped silently (benign, not an error, not skipped). Only a page whose
// ENVELOPE itself is unparseable is fatal (a transport/format fault, not a single bad process).
func (p *DATAJUDParser) ParseBatch(ctx context.Context, pages []RawPayload) (out map[string]ParsedProcess, skipped []string, err error) {
	out = make(map[string]ParsedProcess, len(pages)*datajudBatchPageSize)
	for _, raw := range pages {
		var resp datajudResponse
		if uerr := json.Unmarshal(raw.Body, &resp); uerr != nil {
			return nil, nil, fmt.Errorf("datajud: decode batch payload: %w", uerr)
		}
		for _, hit := range resp.Hits.Hits {
			proc, numero, outcome := p.parseHit(ctx, hit.Source)
			switch outcome {
			case hitParsed:
				out[proc.Record.CNJNumber] = proc
			case hitParseError:
				// A hit DATAJUD returned but we could not parse: a REAL error. Its number stays
				// absent from the map (the caller marks it attempted anyway) but is counted so the
				// fecho lands PARTIAL. An empty numero (unrecoverable) still counts.
				skipped = append(skipped, numero)
			case hitSkippedNoGrade:
				// Benign — no grade to apply, not an error, not skipped-as-failure.
			}
		}
	}
	return out, skipped, nil
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
