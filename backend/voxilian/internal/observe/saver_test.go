package observe

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestSaverMetricsWhitelist(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewSaverMetrics(reg)
	m.SaverLag("character", 1500*time.Millisecond)
	m.SaverLag("item", 250*time.Millisecond)
	m.SaverLag("bank", 0)
	// Unknown strings must not create series.
	m.SaverLag("tos", time.Second)
	m.SaverLag("", time.Second)
	m.SaverLag("character/1", time.Second)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var fam *dto.MetricFamily
	for _, f := range families {
		if f.GetName() == "vox_saver_lag_seconds" {
			fam = f
		}
	}
	if fam == nil {
		t.Fatal("vox_saver_lag_seconds missing")
	}
	if fam.GetType() != dto.MetricType_HISTOGRAM {
		t.Fatalf("type = %v, want HISTOGRAM", fam.GetType())
	}
	seen := map[string]bool{}
	for _, metric := range fam.Metric {
		var agg string
		for _, lp := range metric.Label {
			if lp.GetName() != "aggregate" {
				t.Fatalf("unexpected label %q", lp.GetName())
			}
			agg = lp.GetValue()
		}
		seen[agg] = true
	}
	for _, want := range []string{"character", "item", "bank"} {
		if !seen[want] {
			t.Fatalf("series %q missing (seen %v)", want, seen)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("series = %v, want exactly the three frozen aggregates", seen)
	}
	// Count/sum for the observed kind.
	for _, metric := range fam.Metric {
		for _, lp := range metric.Label {
			if lp.GetValue() == "character" {
				if got := metric.Histogram.GetSampleCount(); got != 1 {
					t.Fatalf("character count = %d, want 1", got)
				}
				if got := metric.Histogram.GetSampleSum(); got != 1.5 {
					t.Fatalf("character sum = %v, want 1.5", got)
				}
			}
		}
	}
}

func TestSaverMetricsBuckets(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewSaverMetrics(reg)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "vox_saver_lag_seconds" {
			continue
		}
		want := prometheus.ExponentialBuckets(0.25, 2, 12)
		for _, metric := range f.Metric {
			bounds := metric.Histogram.Bucket
			if len(bounds) != len(want) {
				t.Fatalf("buckets = %d, want %d", len(bounds), len(want))
			}
			for i, b := range bounds {
				if b.GetUpperBound() != want[i] {
					t.Fatalf("bucket[%d] = %v, want %v", i, b.GetUpperBound(), want[i])
				}
			}
			return
		}
	}
	t.Fatal("vox_saver_lag_seconds missing")
}

func TestSaverMetricsZeroSeriesPrecreated(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewSaverMetrics(reg)
	// No observations at all: the three series must still exist
	// with zero counts.
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]uint64{}
	for _, f := range families {
		if f.GetName() != "vox_saver_lag_seconds" {
			continue
		}
		for _, metric := range f.Metric {
			for _, lp := range metric.Label {
				if lp.GetName() == "aggregate" {
					seen[lp.GetValue()] = metric.Histogram.GetSampleCount()
				}
			}
		}
	}
	for _, want := range []string{"character", "item", "bank"} {
		count, ok := seen[want]
		if !ok {
			t.Fatalf("precreated series %q missing (seen %v)", want, seen)
		}
		if count != 0 {
			t.Fatalf("series %q count = %d, want 0", want, count)
		}
	}
}

func TestObserveServerExposesSaverLag(t *testing.T) {
	srv := New(NewReadiness())
	if srv.SaverMetrics() == nil {
		t.Fatal("nil SaverMetrics accessor")
	}
	srv.SaverMetrics().SaverLag("character", 3*time.Second)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.Handler().ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Result().Body)
	if !strings.Contains(string(body), "vox_saver_lag_seconds") {
		t.Fatalf("/metrics missing vox_saver_lag_seconds:\n%s", body)
	}
	if strings.Contains(string(body), `aggregate="tos"`) {
		t.Fatalf("user-data label leaked:\n%s", body)
	}
}
