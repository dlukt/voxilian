// Package observe wires operational endpoints, metrics, and structured
// logging (spec §10).
//
//   - /healthz reports process liveness ONLY and MUST NOT depend on
//     PostgreSQL (spec explicitly forbids gating liveness on PG).
//   - /readyz reports world+PG+migration readiness; M0 deliberately
//     reports not-ready (nothing behind it exists yet). Later milestones
//     flip readiness via Readiness without replacing this surface.
//   - /metrics serves a dedicated Prometheus registry owned by Server,
//     never the global MustRegister path.
package observe

import (
	"net/http"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Version metadata, overridden at link time (-ldflags -X).
var (
	Version  = "dev"
	Revision = "none"
)

// Readiness is the mutable readiness state. Later milestones set it
// once world load + PG reachability + migration compatibility hold, and
// flip it back on PG outage / shutdown (spec §10). Safe for concurrent
// use; initial state is not-ready.
type Readiness struct {
	ready atomic.Bool
}

// NewReadiness starts not-ready.
func NewReadiness() *Readiness {
	return &Readiness{}
}

// SetReady moves to ready; repeated calls are harmless.
func (r *Readiness) SetReady() {
	r.ready.Store(true)
}

// SetNotReady moves back to not-ready; repeated calls are harmless.
func (r *Readiness) SetNotReady() {
	r.ready.Store(false)
}

// Ready reports current state without blocking.
func (r *Readiness) Ready() bool {
	return r.ready.Load()
}

// Server bundles the operational HTTP surface.
type Server struct {
	readiness *Readiness
	registry  *prometheus.Registry
	outbound  *OutboundMetrics
	mux       *http.ServeMux
}

// New builds the surface with a dedicated registry containing the
// build-info gauge plus the frozen M3-T5b outbound collectors. Further
// gameplay metrics are registered by later milestones on
// Server.Registry().
func New(readiness *Readiness) *Server {
	s := &Server{
		readiness: readiness,
		registry:  prometheus.NewRegistry(),
		mux:       http.NewServeMux(),
	}
	build := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "voxilian_build_info",
			Help: "Voxilian build metadata.",
		},
		[]string{"version", "revision"},
	)
	s.registry.MustRegister(build)
	build.WithLabelValues(Version, Revision).Set(1)
	s.outbound = NewOutboundMetrics(s.registry)
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)
	s.mux.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return s
}

// Registry exposes the dedicated registry for later milestones.
func (s *Server) Registry() *prometheus.Registry { return s.registry }

// OutboundMetrics exposes the frozen outbound observer adapter
// (spec §7.1.12). Future executable wiring passes it as
// gateway.ServerDeps.OutboundObserver; it structurally satisfies the
// gateway observer interface without either package importing the
// other.
func (s *Server) OutboundMetrics() *OutboundMetrics { return s.outbound }

// Handler returns the mux for http.Serve.
func (s *Server) Handler() http.Handler { return s.mux }

func writeText(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// handleHealthz is pure liveness: 200 while the process runs.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeText(w, http.StatusOK, "ok\n")
}

// handleReadyz is 200 only after SetReady; 503 before that.
func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if s.readiness.Ready() {
		writeText(w, http.StatusOK, "ready\n")
		return
	}
	writeText(w, http.StatusServiceUnavailable, "not ready\n")
}
