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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
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
//
// checkTimeout bounds each call into engine. The server's WriteTimeout does
// not cancel the request context, so without this a store whose Redis has
// hung is bounded only by the client library's own retries — which can run
// past WriteTimeout, at which point the fail-closed 503 can no longer be
// written and the client sees a reset instead. The deadline must be shorter
// than WriteTimeout, and engine must honour context cancellation for it to
// bite (RedisStore does: main enables go-redis's context timeouts).
//
// tracer, when non-nil, opens a span around each engine call carrying the
// matched rule and the outcome; the store nests its own Redis and Lua spans
// beneath it, so a cached decision shows as a check span with nothing under
// it and an uncached one shows the round-trip inside. nil disables it at no
// cost to the hot path.
func NewCheckHandler(engine Checker, recorder RequestRecorder, checkTimeout time.Duration, tracer trace.Tracer) http.Handler {
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

		ctx, cancel := context.WithTimeout(r.Context(), checkTimeout)
		defer cancel()
		var span trace.Span
		if tracer != nil {
			ctx, span = tracer.Start(ctx, "limigo.check")
		}
		decision, err := engine.Check(ctx, key, r.Header.Get)
		if span != nil {
			span.SetAttributes(
				attribute.String("limigo.rule", decision.RuleName),
				attribute.Bool("limigo.matched", decision.Matched),
				attribute.Bool("limigo.allowed", decision.Allowed),
			)
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}
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
			setRateLimitPolicyHeader(w, decision)
			setRetryAfterHeader(w, decision.RetryAfter)
			writeJSON(w, http.StatusServiceUnavailable, checkResponse{
				Allowed:      false,
				Matched:      decision.Matched,
				Rule:         decision.RuleName,
				RetryAfterMs: decision.RetryAfter.Milliseconds(),
			})
			return
		}

		status := http.StatusOK
		if decision.Matched && !decision.Allowed {
			status = http.StatusTooManyRequests
		}

		setRateLimitPolicyHeader(w, decision)
		setRateLimitHeader(w, decision)
		setRetryAfterHeader(w, decision.RetryAfter)
		writeJSON(w, status, checkResponse{
			Allowed:      decision.Allowed,
			Matched:      decision.Matched,
			Rule:         decision.RuleName,
			RetryAfterMs: decision.RetryAfter.Milliseconds(),
		})
	})
}

// setRateLimitPolicyHeader sets the IETF RateLimit-Policy field
// (draft-ietf-httpapi-ratelimit-headers-11, section 4): the matched rule's
// static quota as a Structured Field Dictionary keyed by policy name, with
// q the quota in requests and w the window in whole seconds. It is a no-op
// when no rule matched. w is omitted for a token bucket, which has no
// window, and for a window under one second, which the field cannot express
// (w is a non-zero integer of seconds) and which rounding up would misstate.
func setRateLimitPolicyHeader(w http.ResponseWriter, decision rules.Decision) {
	if !decision.Matched {
		return
	}
	value := sfString(decision.RuleName) + ";q=" + strconv.FormatInt(decision.Limit, 10)
	if decision.Window >= time.Second {
		value += ";w=" + strconv.FormatInt(int64(math.Ceil(decision.Window.Seconds())), 10)
	}
	w.Header().Set("RateLimit-Policy", value)
}

// setRateLimitHeader sets the IETF RateLimit field
// (draft-ietf-httpapi-ratelimit-headers-11, section 5): the matched rule's
// current quota state, keyed by the same policy name as RateLimit-Policy,
// with r the remaining requests and t the seconds until the quota is fully
// restored, rounded up like Retry-After so a client never assumes quota
// before it exists. It is a no-op when no rule matched. t is omitted when
// the reset is unknown, which is the case for a locally cached rule; r is
// then this node's local view, not the fleet's (see rules.Decision.Remaining).
func setRateLimitHeader(w http.ResponseWriter, decision rules.Decision) {
	if !decision.Matched {
		return
	}
	value := sfString(decision.RuleName) + ";r=" + strconv.FormatInt(decision.Remaining, 10)
	if decision.Reset > 0 {
		value += ";t=" + strconv.FormatInt(int64(math.Ceil(decision.Reset.Seconds())), 10)
	}
	w.Header().Set("RateLimit", value)
}

// sfString quotes s as an RFC 9651 Structured Field String. Config validation
// already restricts rule names to printable ASCII without the two characters
// sf-string escapes, so the escaping here is belt and braces, not a path the
// service expects to take.
func sfString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('"')
	return b.String()
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
