package api

import (
	"net/http"
	"sync/atomic"
)

// Readiness is the node's willingness to take new work, reported on
// GET /readyz for a load balancer's health check. It starts not-ready, is
// marked Ready once the listeners are up, and is marked Draining at the start
// of shutdown — before the server stops accepting connections — so the proxy
// observes the 503 and routes elsewhere while this node finishes in-flight
// requests. Safe for concurrent use.
//
// Readiness deliberately does not probe Redis. A store outage already
// surfaces as fail-closed 503s on /v1/check, counted as result="error" and
// visible on the dashboard. Failing readiness on it as well would pull every
// node out of the pool at once — the same outage, now as proxy 502s with no
// per-rule metric behind them — and a brief Redis blip would then cost a
// full health-check interval of routing convergence on the way back.
// Readiness answers "is this process able to serve", not "is the
// dependency healthy"; the second question has its own signal.
type Readiness struct {
	ready atomic.Bool
}

// NewReadiness returns a Readiness that reports not-ready until Ready is called.
func NewReadiness() *Readiness {
	return &Readiness{}
}

// Ready marks the node ready: /readyz returns 200.
func (r *Readiness) Ready() {
	r.ready.Store(true)
}

// Draining marks the node as shutting down: /readyz returns 503 from now on.
// Call it before http.Server.Shutdown and leave time for the proxy's next
// health check to observe it; the endpoint changes nothing on its own.
func (r *Readiness) Draining() {
	r.ready.Store(false)
}

// Handler returns the GET /readyz handler.
func (r *Readiness) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !r.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
