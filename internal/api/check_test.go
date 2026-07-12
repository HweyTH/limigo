package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hweyth/limigo/internal/rules"
)

type fakeChecker struct {
	decision       rules.Decision
	err            error
	called         bool
	gotKey         string
	gotHeaderValue string
}

func (c *fakeChecker) Check(ctx context.Context, key string, headerValue func(string) string) (rules.Decision, error) {
	c.called = true
	c.gotKey = key
	c.gotHeaderValue = headerValue("X-Plan")
	return c.decision, c.err
}

func TestNewCheckHandler(t *testing.T) {
	t.Run("HappyPathAllowedResponse", func(t *testing.T) {
		checker := &fakeChecker{decision: rules.Decision{Allowed: true, Matched: true, RuleName: "free-tier"}}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":" user-123 "}`, "free")

		assertStatus(t, recorder, http.StatusOK)
		response := decodeCheckResponse(t, recorder)
		assertResponse(t, response, checkResponse{Allowed: true, Matched: true, Rule: "free-tier"})
		if !checker.called {
			t.Fatal("expected checker to be called")
		}
		if checker.gotKey != "user-123" {
			t.Fatalf("checker key = %q, want user-123", checker.gotKey)
		}
		if checker.gotHeaderValue != "free" {
			t.Fatalf("checker header value = %q, want free", checker.gotHeaderValue)
		}
	})

	t.Run("HappyPathDeniedResponse", func(t *testing.T) {
		checker := &fakeChecker{decision: rules.Decision{Allowed: false, Matched: true, RuleName: "free-tier"}}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":"user-123"}`, "free")

		assertStatus(t, recorder, http.StatusOK)
		response := decodeCheckResponse(t, recorder)
		assertResponse(t, response, checkResponse{Allowed: false, Matched: true, Rule: "free-tier"})
	})

	t.Run("HappyPathUnmatchedResponse", func(t *testing.T) {
		checker := &fakeChecker{decision: rules.Decision{Allowed: false, Matched: false}}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":"user-123"}`, "enterprise")

		assertStatus(t, recorder, http.StatusOK)
		response := decodeCheckResponse(t, recorder)
		assertResponse(t, response, checkResponse{Allowed: false, Matched: false})
	})

	t.Run("HappyPathDeniedLeakyBucketIncludesRetryAfter", func(t *testing.T) {
		checker := &fakeChecker{decision: rules.Decision{
			Allowed:    false,
			Matched:    true,
			RuleName:   "steady-tier",
			RetryAfter: 1500 * time.Millisecond,
		}}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":"user-123"}`, "steady")

		assertStatus(t, recorder, http.StatusOK)
		response := decodeCheckResponse(t, recorder)
		assertResponse(t, response, checkResponse{
			Allowed:      false,
			Matched:      true,
			Rule:         "steady-tier",
			RetryAfterMs: 1500,
		})
		if got := recorder.Header().Get("Retry-After"); got != "2" {
			t.Fatalf("Retry-After header = %q, want 2 (ceil of 1.5s)", got)
		}
	})

	t.Run("HappyPathAllowedResponseOmitsRetryAfter", func(t *testing.T) {
		checker := &fakeChecker{decision: rules.Decision{Allowed: true, Matched: true, RuleName: "steady-tier"}}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":"user-123"}`, "steady")

		assertStatus(t, recorder, http.StatusOK)
		response := decodeCheckResponse(t, recorder)
		assertResponse(t, response, checkResponse{Allowed: true, Matched: true, Rule: "steady-tier"})
		if got := recorder.Header().Get("Retry-After"); got != "" {
			t.Fatalf("Retry-After header = %q, want empty for an allowed request", got)
		}
	})

	t.Run("UnhappyPathInvalidJSON", func(t *testing.T) {
		checker := &fakeChecker{}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":`, "free")

		assertStatus(t, recorder, http.StatusBadRequest)
		if checker.called {
			t.Fatal("expected checker not to be called")
		}
	})

	t.Run("UnhappyPathMissingKey", func(t *testing.T) {
		checker := &fakeChecker{}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":"   "}`, "free")

		assertStatus(t, recorder, http.StatusBadRequest)
		if checker.called {
			t.Fatal("expected checker not to be called")
		}
	})

	t.Run("UnhappyPathWrongMethod", func(t *testing.T) {
		checker := &fakeChecker{}
		recorder := serveCheckRequest(t, checker, http.MethodGet, `{"key":"user-123"}`, "free")

		assertStatus(t, recorder, http.StatusMethodNotAllowed)
		if recorder.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("Allow header = %q, want POST", recorder.Header().Get("Allow"))
		}
		if checker.called {
			t.Fatal("expected checker not to be called")
		}
	})

	t.Run("UnhappyPathStoreErrorFailsClosed", func(t *testing.T) {
		checker := &fakeChecker{
			decision: rules.Decision{Allowed: false, Matched: true, RuleName: "free-tier"},
			err:      errors.New("redis unavailable"),
		}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":"user-123"}`, "free")

		assertStatus(t, recorder, http.StatusServiceUnavailable)
		response := decodeCheckResponse(t, recorder)
		assertResponse(t, response, checkResponse{Allowed: false, Matched: true, Rule: "free-tier"})
	})
}

func serveCheckRequest(t *testing.T, checker *fakeChecker, method string, body string, plan string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, "/v1/check", strings.NewReader(body))
	request.Header.Set("X-Plan", plan)
	recorder := httptest.NewRecorder()

	NewCheckHandler(checker).ServeHTTP(recorder, request)
	return recorder
}

func decodeCheckResponse(t *testing.T, recorder *httptest.ResponseRecorder) checkResponse {
	t.Helper()

	var response checkResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response
}

func assertStatus(t *testing.T, recorder *httptest.ResponseRecorder, want int) {
	t.Helper()

	if recorder.Code != want {
		t.Fatalf("status = %d, want %d; body = %s", recorder.Code, want, recorder.Body.String())
	}
}

func assertResponse(t *testing.T, got checkResponse, want checkResponse) {
	t.Helper()

	if got != want {
		t.Fatalf("response = %+v, want %+v", got, want)
	}
}
