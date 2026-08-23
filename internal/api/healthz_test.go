package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewHealthzHandler(t *testing.T) {
	t.Run("HappyPathReturnsOK", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		recorder := httptest.NewRecorder()

		NewHealthzHandler().ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}
		if body := recorder.Body.String(); body != "" {
			t.Fatalf("body = %q, want empty", body)
		}
	})

	t.Run("UnhappyPathWrongMethod", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/healthz", nil)
		recorder := httptest.NewRecorder()

		NewHealthzHandler().ServeHTTP(recorder, request)

		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusMethodNotAllowed)
		}
		if got := recorder.Header().Get("Allow"); got != http.MethodGet {
			t.Fatalf("Allow header = %q, want GET", got)
		}
	})
}
