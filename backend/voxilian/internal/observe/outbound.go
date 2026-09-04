package observe

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Frozen outbound label dimensions (spec §7.1.12, v0.3.13). The adapter
// whitelists exactly these values: an unexpected internal telemetry
// string is ignored rather than creating an arbitrary new label series
// (accidental high-cardinality metric creation is the worse failure).
// These literals deliberately duplicate the gateway's frozen constants
// as plain strings so this package stays independent of
// internal/gateway — no import in either direction (structural
// OutboundObserver satisfaction).
const (
	outboundLaneCritical = "critical"
	outboundLaneState    = "state"
)

var outboundLanes = map[string]bool{
	outboundLaneCritical: true,
	outboundLaneState:    true,
}

// Session-drop reasons: exactly the four backpressure classifications.
// Forced kicked/session_expired are terminal control traffic, never
// slow-client drop reasons.
var outboundSessionDropReasons = map[string]bool{
	"critical_queue_saturated": true,
	"reliable_enqueue_timeout": true,
	"write_timeout":            true,
	"ack_lag":                  true,
}

// State-drop reasons: exactly the internal T5a classifications.
var outboundStateDropReasons = map[string]bool{
	"evicted":   true,
	"saturated": true,
	"closed":    true,
}

// OutboundMetrics is the Prometheus adapter behind the gateway
// OutboundObserver seam (spec §7.1.12). It implements the observer
// interface structurally — method signatures use base types only — so
// neither package imports the other. Depth and ACK-lag metrics are
// event-sampled histograms (one observation per callback), never
// per-session gauges; no session/account/character/entity/opcode label
// exists anywhere.
type OutboundMetrics struct {
	sessionDrops   *prometheus.CounterVec
	depthMessages  *prometheus.HistogramVec
	depthBytes     *prometheus.HistogramVec
	stateDrops     *prometheus.CounterVec
	stateCoalesced prometheus.Counter
	ackLag         prometheus.Histogram
}

// NewOutboundMetrics constructs and registers the six frozen outbound
// collectors on reg (in production the observe.Server-owned dedicated
// registry — never the global/default registerer). The bounded
// label series are pre-created so /metrics exposes stable zero-valued
// dimensions; no series is created dynamically afterward.
func NewOutboundMetrics(reg prometheus.Registerer) *OutboundMetrics {
	m := &OutboundMetrics{
		sessionDrops: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vox_session_drops_total",
				Help: "Fail-closed slow-client session disconnects by frozen backpressure reason.",
			},
			[]string{"reason"},
		),
		depthMessages: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "vox_outbound_queue_depth_messages",
				Help: "Event-sampled queued lane depth in messages (never includes the active physical write).",
				// 1 .. 65536 (+Inf), matching the message ceiling.
				Buckets: prometheus.ExponentialBuckets(1, 2, 17),
			},
			[]string{"lane"},
		),
		depthBytes: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "vox_outbound_queue_depth_bytes",
				Help: "Event-sampled queued lane depth in complete-frame bytes (never includes the active physical write).",
				// 1 KiB .. 64 MiB (+Inf), matching the byte ceiling.
				Buckets: prometheus.ExponentialBuckets(1024, 2, 17),
			},
			[]string{"lane"},
		),
		stateDrops: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vox_outbound_state_drops_total",
				Help: "Discarded coalescible state messages by frozen internal reason.",
			},
			[]string{"reason"},
		),
		stateCoalesced: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "vox_outbound_state_coalesced_total",
				Help: "Same-key newest-wins coalescible state replacements.",
			},
		),
		ackLag: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Name: "vox_outbound_ack_lag_messages",
				Help: "Event-sampled current unacked IN_WORLD ACK-flow lag in messages.",
				// 1 .. 1,048,576 (+Inf), covering the 1,000,000 window cap.
				Buckets: prometheus.ExponentialBuckets(1, 2, 21),
			},
		),
	}
	for _, reason := range []string{
		"critical_queue_saturated",
		"reliable_enqueue_timeout",
		"write_timeout",
		"ack_lag",
	} {
		m.sessionDrops.WithLabelValues(reason)
	}
	for _, reason := range []string{"evicted", "saturated", "closed"} {
		m.stateDrops.WithLabelValues(reason)
	}
	m.depthMessages.WithLabelValues(outboundLaneCritical)
	m.depthMessages.WithLabelValues(outboundLaneState)
	m.depthBytes.WithLabelValues(outboundLaneCritical)
	m.depthBytes.WithLabelValues(outboundLaneState)
	reg.MustRegister(
		m.sessionDrops,
		m.depthMessages,
		m.depthBytes,
		m.stateDrops,
		m.stateCoalesced,
		m.ackLag,
	)
	return m
}

// QueueDepth implements the gateway OutboundObserver seam: one
// histogram observation per lane per callback. Values mean queued lane
// depth only — the active physical write is deliberately excluded
// (metrics and resident-budget accounting describe different things,
// spec §7.1.12).
func (m *OutboundMetrics) QueueDepth(lane string, messages, bytes int) {
	if !outboundLanes[lane] {
		return
	}
	m.depthMessages.WithLabelValues(lane).Observe(float64(messages))
	m.depthBytes.WithLabelValues(lane).Observe(float64(bytes))
}

// StateDropped implements the observer seam. Unknown internal reasons
// are ignored: they must not create label series.
func (m *OutboundMetrics) StateDropped(reason string) {
	if !outboundStateDropReasons[reason] {
		return
	}
	m.stateDrops.WithLabelValues(reason).Inc()
}

// StateCoalesced implements the observer seam.
func (m *OutboundMetrics) StateCoalesced() {
	m.stateCoalesced.Inc()
}

// SessionDropped implements the observer seam. Unknown internal
// reasons — including terminal-control causes that are not
// backpressure drops — are ignored.
func (m *OutboundMetrics) SessionDropped(reason string) {
	if !outboundSessionDropReasons[reason] {
		return
	}
	m.sessionDrops.WithLabelValues(reason).Inc()
}

// AckLag implements the observer seam: one event-sampled observation
// of the current unacked flow lag. Zero-valued observations fall into
// the first bucket.
func (m *OutboundMetrics) AckLag(messages int) {
	m.ackLag.Observe(float64(messages))
}
