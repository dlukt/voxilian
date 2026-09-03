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

func TestExplicitMissingFile(t *testing.T) {
	clearEnv(t)
	p := filepath.Join(t.TempDir(), "does-not-exist.yaml")
	if _, err := Load(p); err == nil {
		t.Fatal("expected error for explicitly requested missing file")
	}
}
