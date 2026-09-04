package config

import (
	"os"
	"path/filepath"
	"testing"
)

// clearEnv unsets every VOX_* variable the loader reads so ambient
// process environment cannot leak between tests. Callers that need
// variables use t.Setenv (auto-restored).
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"VOX_PG_DSN", "VOX_WS_BIND", "VOX_WORLD_CONSTANTS_PATH",
		"VOX_TICK_HZ", "VOX_SNAPSHOT_INTERVAL_SECONDS",
		"VOX_RATE_MOVE_PER_SEC", "VOX_RATE_INTENT_PER_SEC",
		"VOX_OUTBOUND_MAX_MESSAGES", "VOX_OUTBOUND_MAX_BYTES",
		"VOX_OUTBOUND_RELIABLE_ENQUEUE_TIMEOUT_MS", "VOX_OUTBOUND_WRITE_TIMEOUT_MS",
		"VOX_OUTBOUND_MAX_UNACKED_MESSAGES",
		"VOX_SEED_DATA_DIR", "VOX_LOG_LEVEL",
	} {
		t.Setenv(k, os.Getenv(k)) // snapshot first via Setenv restore chain
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsOnly(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Defaults()
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestFileValues(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "tick_hz: 30\nlog_level: debug\nrate_limits:\n  move_per_sec: 5\n  intent_per_sec: 7\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TickHz != 30 || cfg.LogLevel != "debug" {
		t.Fatalf("file values not applied: %+v", cfg)
	}
	if cfg.RateLimits.MovePerSec != 5 || cfg.RateLimits.IntentPerSec != 7 {
		t.Fatalf("nested file values not applied: %+v", cfg.RateLimits)
	}
	if cfg.WSBind != Defaults().WSBind {
		t.Fatalf("unset file keys must keep defaults: %+v", cfg)
	}
}

func TestPartialNestedFileOverlay(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "rate_limits:\n  move_per_sec: 25\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimits.MovePerSec != 25 || cfg.RateLimits.IntentPerSec != Defaults().RateLimits.IntentPerSec {
		t.Fatalf("partial nested overlay must preserve sibling default: %+v", cfg.RateLimits)
	}

	clearEnv(t)
	p = writeFile(t, "rate_limits:\n  intent_per_sec: 3\n")
	cfg, err = Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimits.IntentPerSec != 3 || cfg.RateLimits.MovePerSec != Defaults().RateLimits.MovePerSec {
		t.Fatalf("partial nested overlay must preserve sibling default: %+v", cfg.RateLimits)
	}
}

func TestEnvOverridesNestedFile(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "rate_limits:\n  move_per_sec: 25\n  intent_per_sec: 7\n")
	t.Setenv("VOX_RATE_MOVE_PER_SEC", "40")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RateLimits.MovePerSec != 40 || cfg.RateLimits.IntentPerSec != 7 {
		t.Fatalf("env must win per nested field: %+v", cfg.RateLimits)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "tick_hz: 30\nlog_level: debug\n")
	t.Setenv("VOX_TICK_HZ", "40")
	t.Setenv("VOX_LOG_LEVEL", "warn")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TickHz != 40 || cfg.LogLevel != "warn" {
		t.Fatalf("env must win over file: %+v", cfg)
	}
}

func TestEnvOverridesDefaults(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VOX_WS_BIND", ":9090")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WSBind != ":9090" {
		t.Fatalf("env must win over defaults: %+v", cfg)
	}
}

func TestMalformedYAML(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "tick_hz: [unclosed\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "tick_hz: 20\nno_such_key: 1\n")
	if _, err := Load(p); err == nil {
		t.Fatal("expected strict-field error")
	}
}

func TestInvalidEnvValues(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	cwd, _ := os.Getwd()
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	for _, tc := range [][2]string{
		{"VOX_TICK_HZ", "fast"},
		{"VOX_SNAPSHOT_INTERVAL_SECONDS", "x"},
		{"VOX_RATE_MOVE_PER_SEC", "1.5"},
		{"VOX_RATE_INTENT_PER_SEC", ""},
		{"VOX_LOG_LEVEL", "verbose"},
		{"VOX_TICK_HZ", "0"}, // parses but fails validation
	} {
		t.Run(tc[0]+"="+tc[1], func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc[0], tc[1])
			if _, err := Load(""); err == nil {
				t.Fatalf("expected error for %s=%q", tc[0], tc[1])
			}
		})
	}
}

// TestOutboundPartialFileOverlay proves an omitted nested outbound key
// retains its default instead of zeroing the whole struct (spec §7.1.1
// config shape, v0.3.12).
func TestOutboundPartialFileOverlay(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "outbound:\n  max_messages: 64\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := Defaults().Outbound
	if cfg.Outbound.MaxMessages != 64 {
		t.Fatalf("max_messages = %d, want 64", cfg.Outbound.MaxMessages)
	}
	if cfg.Outbound.MaxBytes != d.MaxBytes ||
		cfg.Outbound.ReliableEnqueueTimeoutMS != d.ReliableEnqueueTimeoutMS ||
		cfg.Outbound.WriteTimeoutMS != d.WriteTimeoutMS ||
		cfg.Outbound.MaxUnackedMessages != d.MaxUnackedMessages {
		t.Fatalf("partial outbound overlay must preserve sibling defaults: %+v", cfg.Outbound)
	}
}

func TestOutboundFileAllFields(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "outbound:\n"+
		"  max_messages: 32\n"+
		"  max_bytes: 131072\n"+
		"  reliable_enqueue_timeout_ms: 250\n"+
		"  write_timeout_ms: 1500\n"+
		"  max_unacked_messages: 7\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := OutboundConfig{
		MaxMessages:              32,
		MaxBytes:                 131072,
		ReliableEnqueueTimeoutMS: 250,
		WriteTimeoutMS:           1500,
		MaxUnackedMessages:       7,
	}
	if cfg.Outbound != want {
		t.Fatalf("outbound = %+v, want %+v", cfg.Outbound, want)
	}
}

// TestOutboundEnvPrecedence proves env wins per outbound field over
// both file and defaults.
func TestOutboundEnvPrecedence(t *testing.T) {
	clearEnv(t)
	p := writeFile(t, "outbound:\n  max_messages: 64\n  write_timeout_ms: 1500\n")
	t.Setenv("VOX_OUTBOUND_MAX_MESSAGES", "17")
	t.Setenv("VOX_OUTBOUND_MAX_BYTES", "65536")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Outbound.MaxMessages != 17 {
		t.Fatalf("env max_messages = %d, want 17", cfg.Outbound.MaxMessages)
	}
	if cfg.Outbound.MaxBytes != 65536 {
		t.Fatalf("env max_bytes = %d, want 65536", cfg.Outbound.MaxBytes)
	}
	// Untouched file value and untouched default survive.
	if cfg.Outbound.WriteTimeoutMS != 1500 {
		t.Fatalf("file write_timeout_ms = %d, want 1500", cfg.Outbound.WriteTimeoutMS)
	}
	if cfg.Outbound.ReliableEnqueueTimeoutMS != Defaults().Outbound.ReliableEnqueueTimeoutMS {
		t.Fatalf("default enqueue timeout not preserved: %+v", cfg.Outbound)
	}
}

// TestOutboundValidation covers every frozen range boundary (spec
// §7.1.1): each case changes exactly one field from a valid base.
func TestOutboundValidation(t *testing.T) {
	base := Defaults().Outbound
	cases := []struct {
		name string
		set  func(OutboundConfig) OutboundConfig
		ok   bool
	}{
		{"max_messages floor", func(o OutboundConfig) OutboundConfig { o.MaxMessages = 1; return o }, true},
		{"max_messages zero", func(o OutboundConfig) OutboundConfig { o.MaxMessages = 0; return o }, false},
		{"max_messages ceiling ok", func(o OutboundConfig) OutboundConfig { o.MaxMessages = 65535; return o }, true},
		{"max_messages above ceiling", func(o OutboundConfig) OutboundConfig { o.MaxMessages = 65536; return o }, false},
		{"max_bytes floor ok", func(o OutboundConfig) OutboundConfig { o.MaxBytes = 65536; return o }, true},
		{"max_bytes below floor", func(o OutboundConfig) OutboundConfig { o.MaxBytes = 65535; return o }, false},
		{"max_bytes ceiling ok", func(o OutboundConfig) OutboundConfig { o.MaxBytes = 67108864; return o }, true},
		{"max_bytes above ceiling", func(o OutboundConfig) OutboundConfig { o.MaxBytes = 67108865; return o }, false},
		{"enqueue timeout zero", func(o OutboundConfig) OutboundConfig { o.ReliableEnqueueTimeoutMS = 0; return o }, false},
		{"enqueue timeout ceiling ok", func(o OutboundConfig) OutboundConfig { o.ReliableEnqueueTimeoutMS = 60000; return o }, true},
		{"enqueue timeout above ceiling", func(o OutboundConfig) OutboundConfig { o.ReliableEnqueueTimeoutMS = 60001; return o }, false},
		{"write timeout zero", func(o OutboundConfig) OutboundConfig { o.WriteTimeoutMS = 0; return o }, false},
		{"write timeout ceiling ok", func(o OutboundConfig) OutboundConfig { o.WriteTimeoutMS = 60000; return o }, true},
		{"write timeout above ceiling", func(o OutboundConfig) OutboundConfig { o.WriteTimeoutMS = 60001; return o }, false},
		{"max_unacked zero", func(o OutboundConfig) OutboundConfig { o.MaxUnackedMessages = 0; return o }, false},
		{"max_unacked ceiling ok", func(o OutboundConfig) OutboundConfig { o.MaxUnackedMessages = 1000000; return o }, true},
		{"max_unacked above ceiling", func(o OutboundConfig) OutboundConfig { o.MaxUnackedMessages = 1000001; return o }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			o := tc.set(base)
			err := o.validate()
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// TestOutboundInvalidEnvText proves a non-integer VOX_OUTBOUND_* value
// is a descriptive config error, never a silent fallback.
func TestOutboundInvalidEnvText(t *testing.T) {
	for _, k := range []string{
		"VOX_OUTBOUND_MAX_MESSAGES",
		"VOX_OUTBOUND_MAX_BYTES",
		"VOX_OUTBOUND_RELIABLE_ENQUEUE_TIMEOUT_MS",
		"VOX_OUTBOUND_WRITE_TIMEOUT_MS",
		"VOX_OUTBOUND_MAX_UNACKED_MESSAGES",
	} {
		t.Run(k, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(k, "soon")
			if _, err := Load(""); err == nil {
				t.Fatalf("expected error for %s", k)
			}
		})
	}
}

func TestExplicitMissingFile(t *testing.T) {
	clearEnv(t)
	p := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for explicitly requested missing file")
	}
}
