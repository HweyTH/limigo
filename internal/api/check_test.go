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

// fakeRequestRecorder is a RequestRecorder double that records every
// (rule, result) pair passed to RecordRequest, in call order.
type fakeRequestRecorder struct {
	calls []struct{ rule, result string }
}

func (r *fakeRequestRecorder) RecordRequest(rule, result string) {
	r.calls = append(r.calls, struct{ rule, result string }{rule, result})
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

		assertStatus(t, recorder, http.StatusTooManyRequests)
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

		assertStatus(t, recorder, http.StatusTooManyRequests)
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
			decision: rules.Decision{
				Allowed:    false,
				Matched:    true,
				RuleName:   "free-tier",
				RetryAfter: 1500 * time.Millisecond,
			},
			err: errors.New("redis unavailable"),
		}
		recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":"user-123"}`, "free")

		assertStatus(t, recorder, http.StatusServiceUnavailable)
		response := decodeCheckResponse(t, recorder)
		assertResponse(t, response, checkResponse{
			Allowed:      false,
			Matched:      true,
			Rule:         "free-tier",
			RetryAfterMs: 1500,
		})
		if got := recorder.Header().Get("Retry-After"); got != "2" {
			t.Fatalf("Retry-After header = %q, want 2 (ceil of 1.5s) on a 503", got)
		}
	})
}

// TestNewCheckHandlerRecordsRequestOutcome verifies the 4-way result
// classification: error and denied are operationally opposite (fail-closed
// vs. working as designed) and must be recorded under distinct labels, as
// must unmatched (no rule configured for the request) vs. allowed.
func TestNewCheckHandlerRecordsRequestOutcome(t *testing.T) {
	tests := []struct {
		name       string
		decision   rules.Decision
		checkErr   error
		wantRule   string
		wantResult string
	}{
		{
			name:       "Allowed",
			decision:   rules.Decision{Allowed: true, Matched: true, RuleName: "free-tier"},
			wantRule:   "free-tier",
			wantResult: "allowed",
		},
		{
			name:       "Denied",
			decision:   rules.Decision{Allowed: false, Matched: true, RuleName: "free-tier"},
			wantRule:   "free-tier",
			wantResult: "denied",
		},
		{
			name:       "Unmatched",
			decision:   rules.Decision{Allowed: false, Matched: false},
			wantRule:   "",
			wantResult: "unmatched",
		},
		{
			name:       "Error",
			decision:   rules.Decision{Allowed: false, Matched: true, RuleName: "free-tier"},
			checkErr:   errors.New("redis unavailable"),
			wantRule:   "free-tier",
			wantResult: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := &fakeChecker{decision: tt.decision, err: tt.checkErr}
			requestRecorder := &fakeRequestRecorder{}
			request := httptest.NewRequest(http.MethodPost, "/v1/check", strings.NewReader(`{"key":"user-123"}`))
			request.Header.Set("X-Plan", "free")
			responseRecorder := httptest.NewRecorder()

			NewCheckHandler(checker, requestRecorder).ServeHTTP(responseRecorder, request)

			if len(requestRecorder.calls) != 1 {
				t.Fatalf("RecordRequest calls = %d, want 1", len(requestRecorder.calls))
			}
			got := requestRecorder.calls[0]
			if got.rule != tt.wantRule || got.result != tt.wantResult {
				t.Fatalf("RecordRequest(%q, %q), want RecordRequest(%q, %q)", got.rule, got.result, tt.wantRule, tt.wantResult)
			}
		})
	}

	t.Run("MalformedRequestsAreNotRecorded", func(t *testing.T) {
		checker := &fakeChecker{}
		requestRecorder := &fakeRequestRecorder{}
		request := httptest.NewRequest(http.MethodPost, "/v1/check", strings.NewReader(`{"key":"   "}`))
		request.Header.Set("X-Plan", "free")
		responseRecorder := httptest.NewRecorder()

		NewCheckHandler(checker, requestRecorder).ServeHTTP(responseRecorder, request)

		if len(requestRecorder.calls) != 0 {
			t.Fatalf("RecordRequest calls = %d, want 0 for a malformed request", len(requestRecorder.calls))
		}
	})
}

func serveCheckRequest(t *testing.T, checker *fakeChecker, method string, body string, plan string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, "/v1/check", strings.NewReader(body))
	request.Header.Set("X-Plan", plan)
	recorder := httptest.NewRecorder()

	NewCheckHandler(checker, &fakeRequestRecorder{}).ServeHTTP(recorder, request)
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
