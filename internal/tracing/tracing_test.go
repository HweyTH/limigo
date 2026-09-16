package tracing

import (
	"context"
	"strings"
	"testing"
)

// TestSetupRejectsBadInput pins the two ways Setup refuses to start rather
// than silently tracing nowhere: an endpoint that is not a collector URL, and
// a sample ratio outside [0, 1].
func TestSetupRejectsBadInput(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		ratio    float64
		wantErr  string
	}{
		{name: "no host", endpoint: "jaeger", ratio: 1, wantErr: "has no host"},
		{name: "bad scheme", endpoint: "ftp://jaeger:4318", ratio: 1, wantErr: "scheme must be http or https"},
		{name: "ratio above one", endpoint: "http://jaeger:4318", ratio: 1.5, wantErr: "outside [0, 1]"},
		{name: "negative ratio", endpoint: "http://jaeger:4318", ratio: -0.1, wantErr: "outside [0, 1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Setup(context.Background(), tt.endpoint, tt.ratio)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Setup error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestSetupBuildsProviderWithoutConnecting verifies a well-formed endpoint
// yields a provider and a shutdown that completes even though nothing is
// listening: the OTLP/HTTP exporter connects lazily, so Setup must not block
// startup on the collector being up, and shutdown must not hang on it.
func TestSetupBuildsProviderWithoutConnecting(t *testing.T) {
	provider, shutdown, err := Setup(context.Background(), "http://127.0.0.1:1/v1/traces", 0.5)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if provider == nil {
		t.Fatal("provider is nil")
	}
	// A span that is never exported successfully must not make shutdown fail
	// on the test's clock; the exporter's own retry budget is what bounds it.
	_, span := provider.Tracer("test").Start(context.Background(), "probe")
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Logf("shutdown reported the unreachable collector, as it may: %v", err)
	}
}
