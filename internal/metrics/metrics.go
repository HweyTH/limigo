package metrics

import (
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
)

type Metrics struct {
	reg           *prometheus.Registry
	requestsTotal *prometheus.CounterVec
	redisLatency  *prometheus.HistogramVec
}

func New(reg *prometheus.Registry) (*Metrics, error) {
	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "limigo_requests_total",
			Help: "Total check requets by outcome and rule.",
		}, []string{"result", "rule"},
	)
	if err := reg.Register(requestsTotal); err != nil {
		return nil, fmt.Errorf("register requets_total: %w", err)
	}

	return &Metrics{reg: reg, requestsTotal: requestsTotal /* ... */}, nil
}
