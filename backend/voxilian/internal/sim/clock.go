package sim

import "time"

// Ticker is the minimal injectable ticker seam (spec §5.2.3),
// conceptually equivalent to time.Ticker without exposing its full
// surface. Production wraps time.NewTicker; tests use a manual ticker
// so no sleeps are required for tick-loop correctness tests.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock creates tickers (spec §5.2.3). Exact naming is an
// implementation choice; production advances in wall time while tests
// advance manually.
type Clock interface {
	NewTicker(time.Duration) Ticker
}

// systemTicker adapts time.Ticker to the Ticker seam.
type systemTicker struct {
	t *time.Ticker
}

// C returns the tick delivery channel.
func (s *systemTicker) C() <-chan time.Time { return s.t.C }

// Stop releases ticker resources.
func (s *systemTicker) Stop() { s.t.Stop() }

// systemClock is the production Clock wrapping time.NewTicker.
type systemClock struct{}

// NewSystemClock returns the production wall-clock implementation.
func NewSystemClock() Clock { return systemClock{} }

// NewTicker creates a wall-clock ticker with period d.
func (systemClock) NewTicker(d time.Duration) Ticker {
	return &systemTicker{t: time.NewTicker(d)}
}
