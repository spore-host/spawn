package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Registry wraps Prometheus registry
type Registry struct {
	reg *prometheus.Registry
}

// NewRegistry creates a new Registry
func NewRegistry() *Registry {
	return &Registry{
		reg: prometheus.NewRegistry(),
	}
}

// Register registers a collector
func (r *Registry) Register(c prometheus.Collector) error {
	return r.reg.Register(c)
}

// Unregister unregisters a collector
func (r *Registry) Unregister(c prometheus.Collector) bool {
	return r.reg.Unregister(c)
}

// Gatherer returns the registry as a prometheus.Gatherer
func (r *Registry) Gatherer() prometheus.Gatherer {
	return r.reg
}
