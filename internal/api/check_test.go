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

			NewCheckHandler(checker, requestRecorder, testCheckTimeout, nil).ServeHTTP(responseRecorder, request)

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

		NewCheckHandler(checker, requestRecorder, testCheckTimeout, nil).ServeHTTP(responseRecorder, request)

		if len(requestRecorder.calls) != 0 {
			t.Fatalf("RecordRequest calls = %d, want 0 for a malformed request", len(requestRecorder.calls))
		}
	})
}

// testCheckTimeout is generous for the ordinary tests, which never block:
// they need a deadline on the request context but must not race against it.
const testCheckTimeout = 5 * time.Second

// blockingChecker is a Checker double standing in for a store whose Redis
// has gone away: it never answers on its own and returns only when the
// request context ends, with that context's error — the same shape go-redis
// produces once the client honours context deadlines.
type blockingChecker struct{}

func (blockingChecker) Check(ctx context.Context, key string, headerValue func(string) string) (rules.Decision, error) {
	<-ctx.Done()
	return rules.Decision{Matched: true, RuleName: "free-tier"}, ctx.Err()
}

// TestNewCheckHandlerBoundsStoreCallWithDeadline is the hung-Redis case:
// the handler must derive a per-request deadline so a store that blocks
// past it yields the fail-closed 503 inside that deadline, recorded as an
// error, rather than a response that the server's WriteTimeout later turns
// into a connection reset.
func TestNewCheckHandlerBoundsStoreCallWithDeadline(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
	}{
		{name: "50ms", timeout: 50 * time.Millisecond},
		{name: "200ms", timeout: 200 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requestRecorder := &fakeRequestRecorder{}
			request := httptest.NewRequest(http.MethodPost, "/v1/check", strings.NewReader(`{"key":"user-123"}`))
			request.Header.Set("X-Plan", "free")
			responseRecorder := httptest.NewRecorder()

			start := time.Now()
			NewCheckHandler(blockingChecker{}, requestRecorder, tt.timeout, nil).ServeHTTP(responseRecorder, request)
			elapsed := time.Since(start)

			// Well inside the 10s WriteTimeout the deadline exists to beat, and
			// far enough past tt.timeout to absorb scheduler jitter without
			// flaking; the point is bounded, not exact.
			if elapsed > tt.timeout+time.Second {
				t.Fatalf("handler took %s, want within %s of the %s deadline", elapsed, time.Second, tt.timeout)
			}
			assertStatus(t, responseRecorder, http.StatusServiceUnavailable)
			response := decodeCheckResponse(t, responseRecorder)
			assertResponse(t, response, checkResponse{Allowed: false, Matched: true, Rule: "free-tier"})
			if len(requestRecorder.calls) != 1 {
				t.Fatalf("RecordRequest calls = %d, want 1", len(requestRecorder.calls))
			}
			if got := requestRecorder.calls[0]; got.rule != "free-tier" || got.result != "error" {
				t.Fatalf("RecordRequest(%q, %q), want RecordRequest(%q, %q)", got.rule, got.result, "free-tier", "error")
			}
		})
	}
}

func serveCheckRequest(t *testing.T, checker *fakeChecker, method string, body string, plan string) *httptest.ResponseRecorder {
	t.Helper()

	request := httptest.NewRequest(method, "/v1/check", strings.NewReader(body))
	request.Header.Set("X-Plan", plan)
	recorder := httptest.NewRecorder()

	NewCheckHandler(checker, &fakeRequestRecorder{}, testCheckTimeout, nil).ServeHTTP(recorder, request)
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

// TestNewCheckHandlerEmitsRateLimitHeaders pins the IETF RateLimit-Policy
// and RateLimit fields (draft-ietf-httpapi-ratelimit-headers-11) on every
// matched response: the static policy on allowed, denied and fail-closed
// responses alike, the live quota only when a decision was actually made,
// and neither when no rule matched.
func TestNewCheckHandlerEmitsRateLimitHeaders(t *testing.T) {
	tests := []struct {
		name       string
		decision   rules.Decision
		checkErr   error
		wantPolicy string
		wantLimit  string
	}{
		{
			name:       "allowed fixed window carries window and reset",
			decision:   rules.Decision{Allowed: true, Matched: true, RuleName: "free-tier", Limit: 100, Window: time.Minute, Remaining: 57, Reset: 12500 * time.Millisecond},
			wantPolicy: `"free-tier";q=100;w=60`,
			wantLimit:  `"free-tier";r=57;t=13`,
		},
		{
			name:       "denied still reports quota",
			decision:   rules.Decision{Allowed: false, Matched: true, RuleName: "free-tier", Limit: 100, Window: time.Minute, Remaining: 0, Reset: 30 * time.Second},
			wantPolicy: `"free-tier";q=100;w=60`,
			wantLimit:  `"free-tier";r=0;t=30`,
		},
		{
			name:       "token bucket has no window",
			decision:   rules.Decision{Allowed: true, Matched: true, RuleName: "pro-tier", Limit: 1000, Remaining: 999, Reset: 5 * time.Millisecond},
			wantPolicy: `"pro-tier";q=1000`,
			wantLimit:  `"pro-tier";r=999;t=1`,
		},
		{
			name:       "sub-second window cannot be expressed as w",
			decision:   rules.Decision{Allowed: true, Matched: true, RuleName: "fast", Limit: 5, Window: 500 * time.Millisecond, Remaining: 4, Reset: 400 * time.Millisecond},
			wantPolicy: `"fast";q=5`,
			wantLimit:  `"fast";r=4;t=1`,
		},
		{
			name:       "locally cached rule omits an unknown reset",
			decision:   rules.Decision{Allowed: true, Matched: true, RuleName: "cached-tier", Limit: 100, Window: time.Minute, Remaining: 42},
			wantPolicy: `"cached-tier";q=100;w=60`,
			wantLimit:  `"cached-tier";r=42`,
		},
		{
			name:       "store error carries the policy but no live quota",
			decision:   rules.Decision{Allowed: false, Matched: true, RuleName: "free-tier", Limit: 100, Window: time.Minute},
			checkErr:   errors.New("redis unavailable"),
			wantPolicy: `"free-tier";q=100;w=60`,
			wantLimit:  "",
		},
		{
			name:       "unmatched carries neither",
			decision:   rules.Decision{Allowed: false, Matched: false},
			wantPolicy: "",
			wantLimit:  "",
		},
		{
			name:       "policy name is quoted as a structured-field string",
			decision:   rules.Decision{Allowed: true, Matched: true, RuleName: `odd"name\`, Limit: 1, Window: time.Second, Remaining: 0, Reset: time.Second},
			wantPolicy: `"odd\"name\\";q=1;w=1`,
			wantLimit:  `"odd\"name\\";r=0;t=1`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := &fakeChecker{decision: tt.decision, err: tt.checkErr}
			recorder := serveCheckRequest(t, checker, http.MethodPost, `{"key":"user-123"}`, "free")

			if got := recorder.Header().Get("RateLimit-Policy"); got != tt.wantPolicy {
				t.Errorf("RateLimit-Policy = %q, want %q", got, tt.wantPolicy)
			}
			if got := recorder.Header().Get("RateLimit"); got != tt.wantLimit {
				t.Errorf("RateLimit = %q, want %q", got, tt.wantLimit)
			}
		})
	}
}
