package observe

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// metricTypeOf returns the gathered family's type string (COUNTER /
// HISTOGRAM) without naming the dto types directly.
func metricTypeOf(t *testing.T, reg *prometheus.Registry, name string) string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == name {
			return mf.GetType().String()
		}
	}
	t.Fatalf("metric family %q not found", name)
	return ""
}

// labelValuesOf collects the given label's values across one family's
// series.
func labelValuesOf(t *testing.T, reg *prometheus.Registry, name, label string) map[string]int {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]int{}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label {
					out[lp.GetValue()]++
				}
			}
		}
	}
	return out
}

// sampleCountOf returns one histogram series' observation count,
// selected by label value ("" for unlabeled histograms).
func sampleCountOf(t *testing.T, reg *prometheus.Registry, name, label, value string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			match := label == ""
			for _, lp := range m.GetLabel() {
				if lp.GetName() == label && lp.GetValue() == value {
					match = true
				}
			}
			if match {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatalf("series %s{%s=%q} not found", name, label, value)
	return 0
}

// bucketBoundsOf returns the explicit bucket upper bounds of the first
// series of a histogram family.
func bucketBoundsOf(t *testing.T, reg *prometheus.Registry, name string) []float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() == name {
			var out []float64
			for _, b := range mf.GetMetric()[0].GetHistogram().GetBucket() {
				out = append(out, b.GetUpperBound())
			}
			return out
		}
	}
	t.Fatalf("metric family %q not found", name)
	return nil
}

// TestOutboundMetricsFamilies asserts the exact frozen metric family
// names AND types with their full label dimensions and deterministic
// histogram buckets (spec §7.1.12, v0.3.13) — never a loose
// strings.Contains check.
func TestOutboundMetricsFamilies(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewOutboundMetrics(reg)
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	wantType := map[string]string{
		"vox_session_drops_total":            "COUNTER",
		"vox_outbound_queue_depth_messages":  "HISTOGRAM",
		"vox_outbound_queue_depth_bytes":     "HISTOGRAM",
		"vox_outbound_state_drops_total":     "COUNTER",
		"vox_outbound_state_coalesced_total": "COUNTER",
		"vox_outbound_ack_lag_messages":      "HISTOGRAM",
	}
	seen := map[string]bool{}
	for _, mf := range families {
		name := mf.GetName()
		want, ok := wantType[name]
		if !ok {
			t.Errorf("unexpected metric family %q", name)
			continue
		}
		if got := mf.GetType().String(); got != want {
			t.Errorf("%s type = %s, want %s", name, got, want)
		}
		seen[name] = true
	}
	for name := range wantType {
		if !seen[name] {
			t.Errorf("missing metric family %q", name)
		}
	}

	// Frozen label dimensions, pre-created zero-valued (B72).
	got := labelValuesOf(t, reg, "vox_session_drops_total", "reason")
	if len(got) != 4 {
		t.Errorf("session drop reasons = %v, want exactly the four frozen reasons", got)
	}
	for _, reason := range []string{"critical_queue_saturated", "reliable_enqueue_timeout", "write_timeout", "ack_lag"} {
		if got[reason] != 1 {
			t.Errorf("session drop reason %q series = %d, want exactly one", reason, got[reason])
		}
	}
	got = labelValuesOf(t, reg, "vox_outbound_state_drops_total", "reason")
	if len(got) != 3 || got["evicted"] != 1 || got["saturated"] != 1 || got["closed"] != 1 {
		t.Errorf("state drop reasons = %v, want exactly evicted/saturated/closed", got)
	}
	for _, name := range []string{"vox_outbound_queue_depth_messages", "vox_outbound_queue_depth_bytes"} {
		lanes := labelValuesOf(t, reg, name, "lane")
		if len(lanes) != 2 || lanes["critical"] != 1 || lanes["state"] != 1 {
			t.Errorf("%s lanes = %v, want exactly critical/state", name, lanes)
		}
	}

	// Deterministic MVP buckets (B65–B67): first and last explicit
	// bounds plus the bucket count.
	for _, tc := range []struct {
		name        string
		count       int
		first, last float64
	}{
		{"vox_outbound_queue_depth_messages", 17, 1, 65536},
		{"vox_outbound_queue_depth_bytes", 17, 1024, 67108864},
		{"vox_outbound_ack_lag_messages", 21, 1, 1048576},
	} {
		bounds := bucketBoundsOf(t, reg, tc.name)
		if len(bounds) != tc.count {
			t.Errorf("%s bucket count = %d, want %d", tc.name, len(bounds), tc.count)
			continue
		}
		if bounds[0] != tc.first || bounds[len(bounds)-1] != tc.last {
			t.Errorf("%s buckets = %v..%v, want %v..%v",
				tc.name, bounds[0], bounds[len(bounds)-1], tc.first, tc.last)
		}
	}
}

// TestOutboundMetricsCallbacks drives every observer callback and
// proves counts/observations land in the right series (B75).
func TestOutboundMetricsCallbacks(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewOutboundMetrics(reg)
	m.QueueDepth("critical", 3, 4096)
	m.QueueDepth("critical", 0, 0) // zero → first bucket
	m.QueueDepth("state", 1, 1024)
	m.StateDropped("evicted")
	m.StateDropped("closed")
	m.StateCoalesced()
	m.SessionDropped("ack_lag")
	m.AckLag(5)
	m.AckLag(0)

	if v := testutil.ToFloat64(m.sessionDrops.WithLabelValues("ack_lag")); v != 1 {
		t.Errorf("ack_lag drops = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.stateDrops.WithLabelValues("evicted")); v != 1 {
		t.Errorf("evicted = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.stateDrops.WithLabelValues("closed")); v != 1 {
		t.Errorf("closed = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.stateCoalesced); v != 1 {
		t.Errorf("coalesced = %v, want 1", v)
	}
	if n := sampleCountOf(t, reg, "vox_outbound_queue_depth_messages", "lane", "critical"); n != 2 {
		t.Errorf("critical lane depth samples = %d, want 2", n)
	}
	if n := sampleCountOf(t, reg, "vox_outbound_queue_depth_bytes", "lane", "state"); n != 1 {
		t.Errorf("state lane byte samples = %d, want 1", n)
	}
	if n := sampleCountOf(t, reg, "vox_outbound_ack_lag_messages", "", ""); n != 2 {
		t.Errorf("ack lag samples = %d, want 2", n)
	}
}

// TestOutboundMetricsWhitelist proves unknown lane/reason values are
// ignored instead of creating label series (B68–B71, A22) — including
// the deliberately excluded terminal reasons.
func TestOutboundMetricsWhitelist(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewOutboundMetrics(reg)
	for _, bad := range []string{"normal", "", "lane=critical", "CRITICAL"} {
		m.QueueDepth(bad, 1, 1)
	}
	for _, bad := range []string{"kicked", "session_expired", "nope", ""} {
		m.SessionDropped(bad)
	}
	for _, bad := range []string{"timeout", "expired", ""} {
		m.StateDropped(bad)
	}
	if n := testutil.CollectAndCount(m.sessionDrops); n != 4 {
		t.Errorf("session drop series = %d, want exactly 4 (no dynamic labels)", n)
	}
	if n := testutil.CollectAndCount(m.stateDrops); n != 3 {
		t.Errorf("state drop series = %d, want exactly 3", n)
	}
	if n := testutil.CollectAndCount(m.depthMessages); n != 2 {
		t.Errorf("depth message series = %d, want exactly 2", n)
	}
	if n := testutil.CollectAndCount(m.depthBytes); n != 2 {
		t.Errorf("depth byte series = %d, want exactly 2", n)
	}
	// Nothing was counted by the ignored values.
	for name := range map[string]bool{
		"vox_session_drops_total":        true,
		"vox_outbound_state_drops_total": true,
	} {
		if v := metricTypeOf(t, reg, name); v == "" {
			t.Fatalf("%s missing", name)
		}
	}
	if v := testutil.ToFloat64(m.sessionDrops.WithLabelValues("ack_lag")); v != 0 {
		t.Errorf("unknown reason counted: ack_lag = %v, want 0", v)
	}
	if v := testutil.ToFloat64(m.stateDrops.WithLabelValues("evicted")); v != 0 {
		t.Errorf("unknown reason counted: evicted = %v, want 0", v)
	}
	if n := sampleCountOf(t, reg, "vox_outbound_queue_depth_messages", "lane", "critical"); n != 0 {
		t.Errorf("unknown lane counted: %d samples", n)
	}
}

// TestObserveServerExposesOutboundMetrics proves the Server-owned
// dedicated registry registers the outbound collectors and /metrics
// exposes them (B63/B64, spec §7.1.12) — never the global registry.
func TestObserveServerExposesOutboundMetrics(t *testing.T) {
	s := New(NewReadiness())
	if s.OutboundMetrics() == nil {
		t.Fatal("OutboundMetrics() = nil")
	}
	rec := get(t, s.Handler(), "/metrics")
	if rec.Code != 200 {
		t.Fatalf("/metrics = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, name := range []string{
		"vox_session_drops_total",
		"vox_outbound_queue_depth_messages",
		"vox_outbound_queue_depth_bytes",
		"vox_outbound_state_drops_total",
		"vox_outbound_state_coalesced_total",
		"vox_outbound_ack_lag_messages",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics missing %s", name)
		}
	}
	// No high-cardinality labels anywhere (B71).
	for _, banned := range []string{"session=", "account=", "character=", "entity=", "opcode=", "sub=", "ip="} {
		if strings.Contains(body, banned) {
			t.Errorf("/metrics carries banned label %q", banned)
		}
	}
}
