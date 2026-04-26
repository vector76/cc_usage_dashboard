package server

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
)

// Metrics holds in-process counters exposed via GET /metrics in Prometheus
// text format. All counters are thread-safe.
type Metrics struct {
	SnapshotsReceived atomic.Int64
	ParseErrors       atomic.Int64
	SlackQueries      atomic.Int64
	SlackReleases     atomic.Int64

	mu             sync.RWMutex
	eventsIngested map[string]*atomic.Int64
}

// NewMetrics creates a Metrics instance with empty counters.
func NewMetrics() *Metrics {
	return &Metrics{
		eventsIngested: make(map[string]*atomic.Int64),
	}
}

// IncEventsIngested increments events_ingested_total for the given source label.
func (m *Metrics) IncEventsIngested(source string) {
	if source == "" {
		source = "unknown"
	}
	m.mu.RLock()
	c, ok := m.eventsIngested[source]
	m.mu.RUnlock()
	if !ok {
		m.mu.Lock()
		c, ok = m.eventsIngested[source]
		if !ok {
			c = &atomic.Int64{}
			m.eventsIngested[source] = c
		}
		m.mu.Unlock()
	}
	c.Add(1)
}

// EventsIngested returns the current count for a source (for tests).
func (m *Metrics) EventsIngested(source string) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c, ok := m.eventsIngested[source]; ok {
		return c.Load()
	}
	return 0
}

// handleMetrics emits all counters in Prometheus text exposition format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	fmt.Fprintln(w, "# HELP events_ingested_total Total usage events ingested, labeled by source.")
	fmt.Fprintln(w, "# TYPE events_ingested_total counter")
	s.metrics.mu.RLock()
	sources := make([]string, 0, len(s.metrics.eventsIngested))
	for src := range s.metrics.eventsIngested {
		sources = append(sources, src)
	}
	sort.Strings(sources)
	for _, src := range sources {
		fmt.Fprintf(w, "events_ingested_total{source=%q} %d\n", src, s.metrics.eventsIngested[src].Load())
	}
	s.metrics.mu.RUnlock()

	fmt.Fprintln(w, "# HELP snapshots_received_total Total quota snapshots received.")
	fmt.Fprintln(w, "# TYPE snapshots_received_total counter")
	fmt.Fprintf(w, "snapshots_received_total %d\n", s.metrics.SnapshotsReceived.Load())

	fmt.Fprintln(w, "# HELP parse_errors_total Total parse errors recorded.")
	fmt.Fprintln(w, "# TYPE parse_errors_total counter")
	fmt.Fprintf(w, "parse_errors_total %d\n", s.metrics.ParseErrors.Load())

	fmt.Fprintln(w, "# HELP slack_queries_total Total GET /slack queries handled.")
	fmt.Fprintln(w, "# TYPE slack_queries_total counter")
	fmt.Fprintf(w, "slack_queries_total %d\n", s.metrics.SlackQueries.Load())

	fmt.Fprintln(w, "# HELP slack_releases_total Total slack releases recorded.")
	fmt.Fprintln(w, "# TYPE slack_releases_total counter")
	fmt.Fprintf(w, "slack_releases_total %d\n", s.metrics.SlackReleases.Load())
}
