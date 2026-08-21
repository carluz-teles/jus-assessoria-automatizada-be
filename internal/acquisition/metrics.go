package acquisition

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/jusassessoria/platform/lib/obs"
)

// metrics.go holds the import-funnel counters — the aggregate view the per-unit traces
// (see lib/events consumer middleware) cannot give: how many processes/andamentos/
// intimações the import lands, how DATAJUD enrichment fares, and the DJEN status mix
// (the 429 rate). Counters are additive AT THE POINT OF WORK, so N worker replicas sum
// correctly with no double count; they are the metric half of the observability the
// spans started (docs erd-backend §6).

// Import-funnel metric names — one "import." namespace so the backend groups them.
const (
	metricCourtRecords = "import.court_records"       // discovered (attr state=new|seen)
	metricDocketNew    = "import.docket_entries_new"  // NEW andamentos landed
	metricIntimations  = "import.intimations"         // landed (attr state=new|seen)
	metricEnrichment   = "import.enrichments_applied" // DATAJUD merges applied
	metricDiario       = "import.diario_publications" // national firehose landed
	metricMatch        = "import.match_publications"  // firehose folded into tenants
	metricDJENRequests = "import.djen_requests"       // DJEN HTTP calls (attr status_class)
)

// stateNew/stateSeen split a discovered-vs-already-known count so the backend can chart
// genuinely new work apart from re-observations (a re-poll/backfill overlap).
const (
	attrKeyState  = "state"
	attrKeyStatus = "status_class"
	stateNew      = "new"
	stateSeen     = "seen"
)

type importInstruments struct {
	courtRecords metric.Int64Counter
	docketNew    metric.Int64Counter
	intimations  metric.Int64Counter
	enrichment   metric.Int64Counter
	diario       metric.Int64Counter
	match        metric.Int64Counter
	djenRequests metric.Int64Counter
}

// metrics builds the instruments once, lazily, from the global meter (installed by
// telemetry.Setup at boot, before any work runs). OTel returns a USABLE no-op
// instrument alongside a construction error, so telemetry never crashes a worker over a
// bad name — the error is swallowed and the counter just no-ops.
var metrics = sync.OnceValue(func() *importInstruments {
	m := obs.Meter()
	counter := func(name, desc string) metric.Int64Counter {
		inst, _ := m.Int64Counter(name, metric.WithDescription(desc))
		return inst
	}
	return &importInstruments{
		courtRecords: counter(metricCourtRecords, "Court records discovered by the import."),
		docketNew:    counter(metricDocketNew, "New docket entries (andamentos) landed by the import."),
		intimations:  counter(metricIntimations, "Intimations landed by the import."),
		enrichment:   counter(metricEnrichment, "DATAJUD enrichments applied to a discovered record."),
		diario:       counter(metricDiario, "National diário publications landed by ingestion."),
		match:        counter(metricMatch, "Firehose publications folded into tenant intimações."),
		djenRequests: counter(metricDJENRequests, "DJEN HTTP requests, by status class."),
	}
})

// recordSyncTally emits the sync window's landed counts, splitting new from re-observed
// so the funnel charts real discovery apart from overlap. Zero buckets are skipped.
func recordSyncTally(ctx context.Context, t syncTally) {
	m := metrics()
	addSplit(ctx, m.courtRecords, t.CourtRecordsNew, t.CourtRecords-t.CourtRecordsNew)
	addSplit(ctx, m.intimations, t.IntimationsNew, t.Intimations-t.IntimationsNew)
	if t.DocketNew > 0 {
		m.docketNew.Add(ctx, int64(t.DocketNew))
	}
}

// addSplit adds the new/seen buckets of a discovered count under the shared state attr,
// skipping empties so a facet only appears when it carries work.
func addSplit(ctx context.Context, c metric.Int64Counter, newCount, seenCount int) {
	if newCount > 0 {
		c.Add(ctx, int64(newCount), metric.WithAttributes(attribute.String(attrKeyState, stateNew)))
	}
	if seenCount > 0 {
		c.Add(ctx, int64(seenCount), metric.WithAttributes(attribute.String(attrKeyState, stateSeen)))
	}
}

// recordEnrichmentApplied counts one DATAJUD merge that landed on a discovered record.
func recordEnrichmentApplied(ctx context.Context) {
	metrics().enrichment.Add(ctx, 1)
}

// recordEnrichmentBatchApplied counts the n DATAJUD grades one batch step landed — the same
// enrichment counter as the single path, incremented by the step's graded count.
func recordEnrichmentBatchApplied(ctx context.Context, n int) {
	if n > 0 {
		metrics().enrichment.Add(ctx, int64(n))
	}
}

// recordDiarioLanded counts the national publications one tribunal/day landed.
func recordDiarioLanded(ctx context.Context, n int) {
	if n > 0 {
		metrics().diario.Add(ctx, int64(n))
	}
}

// recordMatch counts the firehose publications folded into tenant intimações for a day.
func recordMatch(ctx context.Context, n int) {
	if n > 0 {
		metrics().match.Add(ctx, int64(n))
	}
}

// recordDJENRequest counts one DJEN HTTP call under its status class (2xx|4xx|5xx|429|
// error) — the 429 series is the rate-block signal the DJEN throttle work watches.
func recordDJENRequest(ctx context.Context, statusClass string) {
	metrics().djenRequests.Add(ctx, 1, metric.WithAttributes(attribute.String(attrKeyStatus, statusClass)))
}

// httpStatusClass buckets a status code for the DJEN request counter; 429 is called out
// on its own because a rate block is operationally distinct from a plain 4xx.
func httpStatusClass(code int) string {
	switch {
	case code == 429:
		return "429"
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 200 && code < 300:
		return "2xx"
	default:
		return "other"
	}
}
