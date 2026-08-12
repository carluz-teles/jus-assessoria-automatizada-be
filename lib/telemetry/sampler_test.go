package telemetry

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/jusassessoria/platform/lib/config"
)

// httpAttrs mirrors otelfiber's start-time attributes: the request method under the
// semconv 1.x key the sampler reads.
func httpAttrs(method string) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("http.request.method", method)}
}

func TestImportTraceSampler(t *testing.T) {
	tests := []struct {
		name  string
		span  string
		attrs []attribute.KeyValue
		want  sdktrace.SamplingDecision
	}{
		{name: "POST import trigger sampled", span: "/v1/acquisition/integrations", attrs: httpAttrs(http.MethodPost), want: sdktrace.RecordAndSample},
		{name: "GET on the same path dropped", span: "/v1/acquisition/integrations", attrs: httpAttrs(http.MethodGet), want: sdktrace.Drop},
		{name: "OPTIONS preflight dropped", span: "/", attrs: httpAttrs(http.MethodOptions), want: sdktrace.Drop},
		{name: "polling GET dropped", span: "/v1/notifications/unread-count", attrs: httpAttrs(http.MethodGet), want: sdktrace.Drop},
		{name: "read GET dropped", span: "/v1/processos", attrs: httpAttrs(http.MethodGet), want: sdktrace.Drop},
		{name: "acquisition consumer span sampled", span: "acquisition.sync_requested process", want: sdktrace.RecordAndSample},
		{name: "diario consumer span sampled", span: "acquisition.diario_requested process", want: sdktrace.RecordAndSample},
		{name: "scheduler run_due_poll sampled", span: "scheduler run_due_poll", want: sdktrace.RecordAndSample},
		{name: "scheduler request_day sampled", span: "scheduler request_day", want: sdktrace.RecordAndSample},
		{name: "scheduler match_day sampled", span: "scheduler match_day", want: sdktrace.RecordAndSample},
		{name: "outbox relay publish dropped", span: "outbox relay publish", want: sdktrace.Drop},
		{name: "orphan pgx query dropped", span: "query notifications", want: sdktrace.Drop},
		{name: "non-import consumer dropped", span: "notifications.requested process", want: sdktrace.Drop},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := sdktrace.SamplingParameters{
				ParentContext: context.Background(),
				Name:          tt.span,
				Attributes:    tt.attrs,
			}
			if got := (importTraceSampler{}).ShouldSample(p).Decision; got != tt.want {
				t.Errorf("ShouldSample(%q).Decision = %v, want %v", tt.span, got, tt.want)
			}
		})
	}
}

// buildSampler honors OTEL_TRACES_MODE: only "all" (case-insensitive) restores
// sample-everything; anything else — including a typo or the empty default — stays
// import-only, so a read GET is dropped.
func TestBuildSampler_ModeSwitch(t *testing.T) {
	readGET := sdktrace.SamplingParameters{
		ParentContext: context.Background(),
		Name:          "/v1/processos",
		Attributes:    httpAttrs(http.MethodGet),
	}

	tests := []struct {
		name string
		mode string
		want sdktrace.SamplingDecision
	}{
		{name: "empty default is import-only", mode: "", want: sdktrace.Drop},
		{name: "explicit import-only", mode: "import-only", want: sdktrace.Drop},
		{name: "unknown value falls back to import-only", mode: "banana", want: sdktrace.Drop},
		{name: "all samples everything", mode: "all", want: sdktrace.RecordAndSample},
		{name: "ALL is case-insensitive", mode: "ALL", want: sdktrace.RecordAndSample},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := buildSampler(config.Config{OTELTracesMode: tt.mode})
			if got := s.ShouldSample(readGET).Decision; got != tt.want {
				t.Errorf("mode %q: decision = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}
