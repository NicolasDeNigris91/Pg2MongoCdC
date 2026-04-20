package metrics_test

import (
	"net/http/httptest"
	"strings"
	"testing"

	"zdt/sink/internal/metrics"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The wiring we care about: label names on CounterVec match what the loop
// will call WithLabelValues with. A typo here = silent metric-missing
// production bug. Cheap test, catches the class of bug TDD exists to prevent.
func TestMetrics_EventsProcessedLabelsAndIncrement(t *testing.T) {
	m := metrics.New()
	m.EventsProcessed.WithLabelValues("sink", "users", "c").Inc()
	m.EventsProcessed.WithLabelValues("sink", "users", "c").Inc()
	m.EventsProcessed.WithLabelValues("sink", "users", "u").Inc()
	m.EventsProcessed.WithLabelValues("sink", "orders", "d").Inc()

	if got := testutil.ToFloat64(m.EventsProcessed.WithLabelValues("sink", "users", "c")); got != 2 {
		t.Errorf("users/c: want 2, got %v", got)
	}
	if got := testutil.ToFloat64(m.EventsProcessed.WithLabelValues("sink", "users", "u")); got != 1 {
		t.Errorf("users/u: want 1, got %v", got)
	}
	if got := testutil.ToFloat64(m.EventsProcessed.WithLabelValues("sink", "orders", "d")); got != 1 {
		t.Errorf("orders/d: want 1, got %v", got)
	}
}

// /metrics endpoint serves scrapeable Prometheus text for metrics with samples.
// Note: CounterVec/HistogramVec with zero recorded samples produce no output
// lines (intentional prom behavior). We therefore observe each metric at
// least once before scraping.
func TestMetrics_HTTPHandlerServesRegisteredMetrics(t *testing.T) {
	m := metrics.New()
	m.EventsProcessed.WithLabelValues("sink", "users", "c").Add(42)
	m.IdempotentSkip.WithLabelValues("users").Add(3)
	m.WriteErrors.WithLabelValues("mongo", "network").Add(1)
	m.ReplicationLag.WithLabelValues("users").Observe(1.2)

	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	m.HTTPHandler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"migration_events_processed_total",
		"migration_idempotent_skip_total",
		"migration_replication_lag_seconds",
		"migration_write_errors_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing metric %q in /metrics body", want)
		}
	}
}
