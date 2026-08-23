package api

import "net/http"

// NewHealthzHandler returns an HTTP handler for GET /healthz: the control
// endpoint used to establish a load test's ceiling (ADR-0004). It must stay
// trivial forever — no body parsing, no rule evaluation, no store access, no
// dependency probing. Anything added here (a Redis ping, a readiness check)
// silently contaminates every published measurement that uses it as a
// baseline; real readiness reporting belongs on a separate endpoint.
func NewHealthzHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
