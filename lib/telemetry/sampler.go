// sampler.go implements the process trace sampler: deny-by-default, keeping ONLY the
// import flow (the flagship pipeline) so the backend shows it clean instead of drowned
// in CORS preflight, polling and read spans — which also cuts ingest cost. It relies on
// two facts: dropping a root's decision cascades to its children (ParentBased), and each
// import unit is its OWN trace linked to its producer (see lib/events consumer
// middleware). So allowlisting the import ROOTS covers the whole async fan-out.
// OTEL_TRACES_MODE=all restores sample-everything for incident debugging
// (config.TracesImportOnly).
package telemetry

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/jusassessoria/platform/lib/config"
)

const (
	// importIntegrationsPath is the ONE on-demand import trigger. otelfiber names the
	// HTTP span with the request path at span start (before route templating), so the
	// sampler matches this concrete path; the method attribute pins it to the POST — the
	// GET on the same path is a read and is dropped.
	importIntegrationsPath = "/v1/acquisition/integrations"

	// attrHTTPMethod is the start-time method attribute otelfiber v2 sets (semconv 1.x);
	// attrHTTPMethodOld is the pre-1.x key, matched as a fallback so a semconv bump on
	// either side never silently starts sampling every route.
	attrHTTPMethod    = "http.request.method"
	attrHTTPMethodOld = "http.method"
)

// importSpanPrefixes are the span-name prefixes that belong to the import flow: every
// acquisition event-consumer span ("acquisition.<evt> process", each a new root linked
// to its producer) and the scheduler's periodic import roots ("scheduler <job>").
var importSpanPrefixes = []string{"acquisition.", "scheduler "}

// importTraceSampler keeps a root span only when it belongs to the import flow. It is a
// ROOT sampler (wrapped in ParentBased): local children and consumer links inherit the
// root's decision, so allowlisting the roots traces the whole pipeline and drops the rest.
type importTraceSampler struct{}

func (importTraceSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	psc := trace.SpanContextFromContext(p.ParentContext)
	decision := sdktrace.Drop
	if importAllowed(p.Name, p.Attributes) {
		decision = sdktrace.RecordAndSample
	}
	// Carry the parent trace state through, mirroring the built-in SDK samplers.
	return sdktrace.SamplingResult{Decision: decision, Tracestate: psc.TraceState()}
}

func (importTraceSampler) Description() string { return "ImportTraceSampler{deny-by-default}" }

// importAllowed is the allowlist: an import span-name prefix, or the single POST that
// triggers an on-demand import.
func importAllowed(name string, attrs []attribute.KeyValue) bool {
	for _, prefix := range importSpanPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return name == importIntegrationsPath && httpMethod(attrs) == http.MethodPost
}

// httpMethod reads the request method off the span's start attributes; empty when the
// span is not an HTTP server span.
func httpMethod(attrs []attribute.KeyValue) string {
	for _, a := range attrs {
		if key := string(a.Key); key == attrHTTPMethod || key == attrHTTPMethodOld {
			return a.Value.AsString()
		}
	}
	return ""
}

// buildSampler picks the process trace sampler from config. Default (import-only) keeps
// only the import flow via importTraceSampler; OTEL_TRACES_MODE=all restores
// sample-everything for incident debugging. Both wrap in ParentBased so a local child
// inherits its root's decision; in import-only mode a REMOTE parent (e.g. an HTTP root
// the frontend traced) is re-judged by the same policy, so the backend stays
// authoritative (deny-by-default) rather than trusting an upstream sampled flag.
func buildSampler(cfg config.Config) sdktrace.Sampler {
	if !cfg.TracesImportOnly() {
		return sdktrace.ParentBased(sdktrace.AlwaysSample())
	}
	root := importTraceSampler{}
	return sdktrace.ParentBased(
		root,
		sdktrace.WithRemoteParentSampled(root),
		sdktrace.WithRemoteParentNotSampled(root),
	)
}
