package observe

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthzAlwaysOK(t *testing.T) {
	s := New(NewReadiness()) // never flipped: healthz must not care
	rec := get(t, s.Handler(), "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("healthz content-type = %q", ct)
	}
}

func TestReadyzGate(t *testing.T) {
	r := NewReadiness()
	s := New(r)
	if rec := get(t, s.Handler(), "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before ready = %d, want 503", rec.Code)
	}
	r.SetReady()
	r.SetReady() // idempotent
	if rec := get(t, s.Handler(), "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("readyz after ready = %d, want 200", rec.Code)
	}
}

func TestMetricsBuildInfo(t *testing.T) {
	s := New(NewReadiness())
	rec := get(t, s.Handler(), "/metrics")
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "voxilian_build_info") {
		t.Fatalf("metrics missing voxilian_build_info:\n%s", body)
	}
}

func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"debug", "DEBUG"}, {"INFO", "INFO"}, {" Warn ", "WARN"}, {"error", "ERROR"},
	} {
		lvl, err := ParseLevel(tc.in)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", tc.in, err)
		}
		if lvl.String() != tc.want {
			t.Fatalf("ParseLevel(%q) = %s, want %s", tc.in, lvl, tc.want)
		}
	}
	if _, err := ParseLevel("verbose"); err == nil {
		t.Fatal("expected error for verbose")
	}
}

func TestLoggerFieldsAndLevel(t *testing.T) {
	var buf bytes.Buffer
	l, err := NewLogger(&buf, "warn")
	if err != nil {
		t.Fatal(err)
	}
	l = WithTick(WithCell(WithCharID(l, 42), 3, -7), 99)
	l.Info("dropped by level")
	l.Warn("kept", "k", "v")
	out := buf.String()
	if strings.Contains(out, "dropped by level") {
		t.Fatalf("info must not pass warn level:\n%s", out)
	}
	for _, want := range []string{`"tick":99`, `"charID":42`, `"k":"v"`, "kept"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in:\n%s", want, out)
		}
	}
	if _, err := NewLogger(io.Discard, "verbose"); err == nil {
		t.Fatal("expected error for bad level")
	}
}
