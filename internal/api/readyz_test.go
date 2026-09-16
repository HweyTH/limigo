package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestReadiness pins the liveness/readiness split: /readyz reports 200 only
// while the node is willing to take new work, and 503 once shutdown has
// begun, so a proxy stops routing to a draining replica. /healthz is left
// untouched by design (see NewHealthzHandler).
func TestReadiness(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		ready      bool
		wantStatus int
	}{
		{name: "not ready before Ready() is called", method: http.MethodGet, ready: false, wantStatus: http.StatusServiceUnavailable},
		{name: "ready", method: http.MethodGet, ready: true, wantStatus: http.StatusOK},
		{name: "wrong method while ready", method: http.MethodPost, ready: true, wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readiness := NewReadiness()
			if tt.ready {
				readiness.Ready()
			}
			request := httptest.NewRequest(tt.method, "/readyz", nil)
			recorder := httptest.NewRecorder()

			readiness.Handler().ServeHTTP(recorder, request)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusMethodNotAllowed {
				if got := recorder.Header().Get("Allow"); got != http.MethodGet {
					t.Fatalf("Allow header = %q, want GET", got)
				}
			}
		})
	}

	t.Run("Draining flips a ready node to 503 and stays there", func(t *testing.T) {
		readiness := NewReadiness()
		readiness.Ready()
		readiness.Draining()

		for i := 0; i < 2; i++ {
			request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			recorder := httptest.NewRecorder()
			readiness.Handler().ServeHTTP(recorder, request)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status after Draining = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
			}
		}
	})
}
