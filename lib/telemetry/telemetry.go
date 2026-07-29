// Package telemetry wires OpenTelemetry for every binary: traces and metrics
// exported over OTLP/gRPC, plus a slog handler (see slog.go) that stamps the
// active trace_id/span_id onto log records so logs correlate with traces.
//
// Setup runs once per process, at the "pools/asynq/otel" boot step (docs
// erd-backend §6). It installs the global TracerProvider, MeterProvider and a
// W3C propagator, then returns a shutdown closure the graceful-shutdown path
// invokes during drain. The OTLP/gRPC exporters are lazy — New does not dial —
// so Setup returns fast even when the collector is unreachable; export failures
// surface later on the batch/periodic path, not here.
//
// The W3C traceparent propagator is deliberate: the outbox relay carries
// trace_context in the event payload so a trace spans the async hop from
// producer to listener (docs erd-backend §6, transactional outbox).
package telemetry

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"

	"github.com/jusassessoria/platform/lib/apperr"
	"github.com/jusassessoria/platform/lib/config"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"google.golang.org/grpc/credentials"
)

// serviceVersion is reported on every resource. v0 · fundação; bump on release.
const serviceVersion = "0.1.0"

// Setup installs the global OpenTelemetry providers and propagator for the
// process and returns a shutdown closure. serviceName identifies the binary
// (e.g. "api", "worker-outbox-relay") in the telemetry backend. The returned
// shutdown flushes and stops both providers; call it during graceful shutdown.
func Setup(ctx context.Context, cfg config.Config, serviceName string) (shutdown func(context.Context) error, err error) {
	res, err := newResource(serviceName, cfg.Env)
	if err != nil {
		return nil, err
	}

	// Trace e metric exporters compartilham a MESMA decisão de segurança/headers
	// (uma só fonte de verdade): insecure vs TLS 1.2 e os cabeçalhos de auth.
	sec := otlpSecurityFrom(cfg)

	// gRPC exporters are lazy: New wires the client but does not dial, so a down
	// collector never blocks boot.
	traceExp, err := otlptracegrpc.New(ctx, otlpOptions(
		cfg.OTELEndpoint, sec,
		otlptracegrpc.WithEndpoint,
		otlptracegrpc.WithInsecure,
		otlptracegrpc.WithTLSCredentials,
		otlptracegrpc.WithHeaders,
	)...)
	if err != nil {
		return nil, apperr.NewInfra("telemetry: build trace exporter", err)
	}

	metricExp, err := otlpmetricgrpc.New(ctx, otlpOptions(
		cfg.OTELEndpoint, sec,
		otlpmetricgrpc.WithEndpoint,
		otlpmetricgrpc.WithInsecure,
		otlpmetricgrpc.WithTLSCredentials,
		otlpmetricgrpc.WithHeaders,
	)...)
	if err != nil {
		// traceExp is lazy and never dialed; shut it down anyway so we do not
		// leak the client if this rare config error fires.
		_ = traceExp.Shutdown(ctx)
		return nil, apperr.NewInfra("telemetry: build metric exporter", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp),
		sdktrace.WithResource(res),
	)
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)

	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdown = func(ctx context.Context) error {
		// Join both shutdowns so a failure in one still stops the other; the
		// caller sees a single typed infra error.
		if err := errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)); err != nil {
			return apperr.NewInfra("telemetry: shutdown providers", err)
		}
		return nil
	}
	return shutdown, nil
}

// otlpSecurity é a decisão de segurança/headers que trace e metric exporters
// aplicam de forma IDÊNTICA. insecure liga o dial sem TLS (collector local);
// caso contrário creds exige TLS 1.2 (backends autenticados como o New Relic).
// headers carrega os cabeçalhos de auth já parseados (ex.: `api-key`), vazio
// quando nenhum foi configurado.
type otlpSecurity struct {
	insecure bool
	creds    credentials.TransportCredentials
	headers  map[string]string
}

// otlpSecurityFrom deriva a decisão de segurança da Config uma única vez. Fora do
// modo insecure constrói credenciais TLS com mínimo TLS 1.2 (exigência do New
// Relic e da spec OTLP quando INSECURE=false).
func otlpSecurityFrom(cfg config.Config) otlpSecurity {
	sec := otlpSecurity{
		insecure: cfg.OTELInsecure,
		headers:  parseOTLPHeaders(cfg.OTELHeaders),
	}
	if !sec.insecure {
		sec.creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	return sec
}

// otlpOptions monta as opções comuns dos dois exporters OTLP/gRPC (endpoint +
// segurança + headers) a partir de uma só decisão. Os tipos Option de trace e
// metric diferem, então os construtores entram como parâmetros; a montagem
// (ordem e condições) vive num lugar só — sem repetir a lógica entre os dois.
func otlpOptions[O any](
	endpoint string,
	sec otlpSecurity,
	withEndpoint func(string) O,
	withInsecure func() O,
	withTLS func(credentials.TransportCredentials) O,
	withHeaders func(map[string]string) O,
) []O {
	opts := []O{withEndpoint(endpoint)}
	if sec.insecure {
		opts = append(opts, withInsecure())
	} else {
		opts = append(opts, withTLS(sec.creds))
	}
	if len(sec.headers) > 0 {
		opts = append(opts, withHeaders(sec.headers))
	}
	return opts
}

// parseOTLPHeaders converte a string de headers no formato da spec OTLP
// (`key1=value1,key2=value2`) num mapa. Divide os pares por vírgula e cada par no
// PRIMEIRO `=`, então um valor pode conter `=` (ex.: `k=a=b` → {k: "a=b"}).
// Espaços em volta de chave e valor são aparados; pares em branco, sem `=` ou sem
// chave são ignorados. String vazia devolve um mapa vazio.
func parseOTLPHeaders(raw string) map[string]string {
	headers := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue // malformado: par sem `=`
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		headers[key] = strings.TrimSpace(value)
	}
	return headers
}

// newResource describes this process to the telemetry backend: service name,
// version and deployment environment, merged over the SDK defaults (host,
// process, SDK info). The semconv version here matches resource.Default's
// schema URL so the merge does not raise a schema conflict.
func newResource(serviceName, env string) (*resource.Resource, error) {
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(serviceVersion),
			semconv.DeploymentEnvironmentNameKey.String(env),
		),
	)
	if err != nil {
		return nil, apperr.NewInfra("telemetry: build resource", err)
	}
	return res, nil
}
