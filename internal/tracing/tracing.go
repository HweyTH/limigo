// Package tracing configures OpenTelemetry tracing for Limigo: an OTLP/HTTP
// exporter pointed at a collector such as Jaeger, behind a tracer provider
// the rest of the service receives by injection. Nothing here touches the
// OpenTelemetry globals; a caller that does not call Setup gets no tracing
// and pays nothing for it.
package tracing

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// ServiceName is the service.name resource attribute every span carries.
const ServiceName = "limigo"

// Setup builds a tracer provider that exports spans over OTLP/HTTP to
// endpoint, a URL such as http://jaeger:4318 (the scheme selects TLS; a path,
// if given, replaces the default /v1/traces). sampleRatio in [0, 1] is the
// fraction of new traces recorded; traces that arrive with a sampled parent
// are always recorded, so an upstream decision is honoured.
//
// The returned shutdown flushes pending spans and stops the exporter; call it
// on the way out or the tail of the last batch is lost.
func Setup(ctx context.Context, endpoint string, sampleRatio float64) (provider *sdktrace.TracerProvider, shutdown func(context.Context) error, err error) {
	if sampleRatio < 0 || sampleRatio > 1 {
		return nil, nil, fmt.Errorf("sample ratio %v is outside [0, 1]", sampleRatio)
	}
	opts, err := exporterOptions(endpoint)
	if err != nil {
		return nil, nil, err
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP trace exporter for %q: %w", endpoint, err)
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(ServiceName),
		semconv.ServiceInstanceID(hostname),
	))
	if err != nil {
		return nil, nil, fmt.Errorf("build trace resource: %w", err)
	}

	provider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRatio))),
	)
	shutdown = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := provider.Shutdown(ctx); err != nil {
			return fmt.Errorf("shut down tracer provider: %w", err)
		}
		return nil
	}
	return provider, shutdown, nil
}

// exporterOptions turns a collector URL into exporter options. The exporter's
// own WithEndpointURL uses the URL path verbatim and does not append
// /v1/traces, which makes the natural "http://jaeger:4318" silently post to
// the wrong path; splitting the URL ourselves keeps the default path unless
// the caller gives one.
func exporterOptions(endpoint string) ([]otlptracehttp.Option, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse tracing endpoint %q: %w", endpoint, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("tracing endpoint %q has no host; want a URL such as http://jaeger:4318", endpoint)
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(u.Host)}
	switch u.Scheme {
	case "http":
		opts = append(opts, otlptracehttp.WithInsecure())
	case "https":
	default:
		return nil, fmt.Errorf("tracing endpoint %q: scheme must be http or https", endpoint)
	}
	if u.Path != "" && u.Path != "/" {
		opts = append(opts, otlptracehttp.WithURLPath(u.Path))
	}
	return opts, nil
}
