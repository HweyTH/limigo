package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hweyth/limigo/internal/rules"
)

// Checker is the request-time rule engine used by the HTTP API.
type Checker interface {
	Check(ctx context.Context, key string, headerValue func(string) string) (rules.Decision, error)
}

type checkRequest struct {
	Key string `json:"key"`
}

type checkResponse struct {
	Allowed bool   `json:"allowed"`
	Matched bool   `json:"matched"`
	Rule    string `json:"rule,omitempty"`
}

// NewCheckHandler returns an HTTP handler for POST /v1/check requests.
func NewCheckHandler(engine Checker) http.Handler {
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
		if err != nil {
			writeJSON(w, http.StatusServiceUnavailable, checkResponse{
				Allowed: false,
				Matched: decision.Matched,
				Rule:    decision.RuleName,
			})
			return
		}

		writeJSON(w, http.StatusOK, checkResponse{
			Allowed: decision.Allowed,
			Matched: decision.Matched,
			Rule:    decision.RuleName,
		})
	})
}

func writeJSON(w http.ResponseWriter, status int, response checkResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(response)
}
