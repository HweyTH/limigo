package api

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hweyth/limigo/internal/rules"
)

// Checker is the request-time rule engine used by the HTTP API.
type Checker interface {
	Check(ctx context.Context, key string, headerValue func(string) string) (rules.Decision, error)
}

// RequestRecorder receives the outcome of every completed check, labeled by
// rule and a 4-way result: allowed, denied, unmatched, or error. error and
// denied are recorded separately because they are operationally opposite —
// error means the backing store failed and requests are being fail-closed,
// denied means the rate limiter worked as designed.
type RequestRecorder interface {
	RecordRequest(rule, result string)
}

type checkRequest struct {
	Key string `json:"key"`
}

type checkResponse struct {
	Allowed bool   `json:"allowed"`
	Matched bool   `json:"matched"`
	Rule    string `json:"rule,omitempty"`
	// RetryAfterMs is the exact number of milliseconds until the matched
	// rule will next admit a request, when the algorithm can compute it
	// (currently only leaky bucket). Omitted when not applicable.
	RetryAfterMs int64 `json:"retry_after_ms,omitempty"`
}

// NewCheckHandler returns an HTTP handler for POST /v1/check requests.
// recorder is notified of every completed check's outcome; malformed
// requests that never reach engine (bad method, bad body, empty key) are not
// recorded, since they are not rate-limit decisions.
func NewCheckHandler(engine Checker, recorder RequestRecorder) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req checkRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, checkResponse{Allowed: false, Matched: false})
			return
		}

		key := strings.TrimSpace(req.Key)
		if key == "" {
			writeJSON(w, http.StatusBadRequest, checkResponse{Allowed: false, Matched: false})
			return
		}

		decision, err := engine.Check(r.Context(), key, r.Header.Get)
		switch {
		case err != nil:
			recorder.RecordRequest(decision.RuleName, "error")
		case !decision.Matched:
			recorder.RecordRequest("", "unmatched")
		case decision.Allowed:
			recorder.RecordRequest(decision.RuleName, "allowed")
		default:
			recorder.RecordRequest(decision.RuleName, "denied")
		}

		if err != nil {
			setRetryAfterHeader(w, decision.RetryAfter)
			writeJSON(w, http.StatusServiceUnavailable, checkResponse{
				Allowed:      false,
				Matched:      decision.Matched,
				Rule:         decision.RuleName,
				RetryAfterMs: decision.RetryAfter.Milliseconds(),
			})
			return
		}

		setRetryAfterHeader(w, decision.RetryAfter)
		writeJSON(w, http.StatusOK, checkResponse{
			Allowed:      decision.Allowed,
			Matched:      decision.Matched,
			Rule:         decision.RuleName,
			RetryAfterMs: decision.RetryAfter.Milliseconds(),
		})
	})
}

// setRetryAfterHeader sets the standard Retry-After header, rounded up to
// whole seconds so a client never retries before the schedule allows it. It
// is a no-op when retryAfter is zero (not applicable or request was allowed).
func setRetryAfterHeader(w http.ResponseWriter, retryAfter time.Duration) {
	if retryAfter <= 0 {
		return
	}
	seconds := int64(math.Ceil(retryAfter.Seconds()))
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}

func writeJSON(w http.ResponseWriter, status int, response checkResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
