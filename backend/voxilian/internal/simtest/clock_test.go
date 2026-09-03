package simtest

import (
	"sync"
	"testing"
	"time"
)

func TestClockExact(t *testing.T) {
	start := time.Date(2026, 9, 3, 20, 0, 0, 0, time.UTC)
	c := NewClock(start)
	if got := c.Now(); !got.Equal(start) {
		t.Fatalf("initial time = %v, want %v", got, start)
	}
	if got := c.Advance(1500 * time.Millisecond); !got.Equal(start.Add(1500 * time.Millisecond)) {
		t.Fatalf("after advance = %v", got)
	}
	if got := c.Now(); !got.Equal(start.Add(1500 * time.Millisecond)) {
		t.Fatalf("Now after advance = %v", got)
	}
}

func TestClockDeterministic(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a, b := NewClock(start), NewClock(start)
	steps := []time.Duration{time.Second, 20 * time.Millisecond, time.Hour, 0}
	for _, d := range steps {
		if x, y := a.Advance(d), b.Advance(d); !x.Equal(y) {
			t.Fatalf("divergence after %v: %v vs %v", d, x, y)
		}
	}
}

func TestClockConcurrent(t *testing.T) {
	c := NewClock(time.Unix(0, 0).UTC())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Advance(time.Millisecond)
				_ = c.Now()
			}
		}()
	}
	wg.Wait()
	if want := time.Unix(0, 0).UTC().Add(800 * time.Millisecond); !c.Now().Equal(want) {
		t.Fatalf("concurrent total = %v, want %v", c.Now(), want)
	}
}
