package observe

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Frozen saver-lag label values (spec §8.3.14): exactly the three
// aggregate kinds. Unknown telemetry strings are ignored rather
// than creating arbitrary new label series.
var saverLagAggregates = map[string]bool{
	"character": true,
	"item":      true,
	"bank":      true,
}

// SaverMetrics is the Prometheus adapter behind the sim
// SaverObserver seam (spec §8.3.14). It implements the observer
// structurally — SaverLag(string, time.Duration) uses base types
// only — so neither package imports the other.
type SaverMetrics struct {
	lag *prometheus.HistogramVec
}

// NewSaverMetrics constructs and registers the frozen
// vox_saver_lag_seconds histogram on reg (in production the
// observe.Server-owned dedicated registry — never the
// global/default registerer). The three bounded aggregate series
// are pre-created so /metrics exposes stable zero-valued
// dimensions; no series is created dynamically afterward.
func NewSaverMetrics(reg prometheus.Registerer) *SaverMetrics {
	m := &SaverMetrics{
		lag: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "vox_saver_lag_seconds",
				Help:    "Snapshot age at successful persistence acknowledgement, in seconds (capture/enqueue time to accepted CAS success).",
				Buckets: prometheus.ExponentialBuckets(0.25, 2, 12),
			},
			[]string{"aggregate"},
		),
	}
	for _, agg := range []string{"character", "item", "bank"} {
		m.lag.WithLabelValues(agg)
	}
	reg.MustRegister(m.lag)
	return m
}

// SaverLag implements the sim observer seam: one histogram
// observation per accepted CAS success. Unknown aggregate strings
// are ignored and must not create label series.
func (m *SaverMetrics) SaverLag(aggregate string, lag time.Duration) {
	if !saverLagAggregates[aggregate] {
		return
	}
	m.lag.WithLabelValues(aggregate).Observe(lag.Seconds())
}
