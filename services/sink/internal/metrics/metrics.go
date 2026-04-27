// Package metrics owns the Prometheus counters/gauges/histograms exposed
// by the sink service. Keeping them in a single place makes /metrics
// output auditable against the SLO table in docs/plan.md.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics bundles every collector the sink exposes. The zero value is
// NOT usable - always use New.
type Metrics struct {
	Reg *prometheus.Registry

	// events_processed_total{stage,table,op} - the throughput counter.
	EventsProcessed *prometheus.CounterVec

	// write_errors_total{sink,reason} - Mongo write failures by category.
	WriteErrors *prometheus.CounterVec

	// replication_lag_seconds{table} - now() - event.source_ts at sink commit.
	ReplicationLag *prometheus.HistogramVec

	// idempotent_skip_total{table} - LSN-gate rejections (E11000 swallows,
	// tombstones skipped). Expected non-zero under replay; anomaly otherwise.
	IdempotentSkip *prometheus.CounterVec
}

// New creates a Metrics with all collectors registered in a fresh registry.
// Tests use a dedicated registry per test so label cardinality cannot leak
// across tests.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Reg: reg,
		EventsProcessed: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "migration_events_processed_total",
				Help: "CDC events applied by stage/table/op.",
			},
			[]string{"stage", "table", "op"},
		),
		WriteErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "migration_write_errors_total",
				Help: "Downstream write failures by sink and reason.",
			},
			[]string{"sink", "reason"},
		),
		ReplicationLag: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "migration_replication_lag_seconds",
				Help:    "End-to-end lag from Postgres commit to downstream write.",
				Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
			},
			[]string{"table"},
		),
		IdempotentSkip: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "migration_idempotent_skip_total",
				Help: "Events that were acknowledged as no-ops by the LSN gate.",
			},
			[]string{"table"},
		),
	}
	reg.MustRegister(m.EventsProcessed, m.WriteErrors, m.ReplicationLag, m.IdempotentSkip)
	return m
}

// HTTPHandler returns the http.Handler for /metrics scraping.
func (m *Metrics) HTTPHandler() http.Handler {
	return promhttp.HandlerFor(m.Reg, promhttp.HandlerOpts{})
}
