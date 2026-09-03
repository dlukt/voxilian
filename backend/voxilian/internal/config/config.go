// Package config loads the Voxilian server configuration (spec §10).
//
// Precedence is strict and total:
//
//	defaults < config.yaml file < VOX_* environment variables
//
// A missing config.yaml at the default path is acceptable; an explicitly
// requested path that does not exist is an error. Every parse/validation
// failure returns a descriptive error and never silently falls back.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// RateLimitConfig carries per-character inbound caps (spec §7).
type RateLimitConfig struct {
	// MovePerSec caps 102 move intents per second.
	MovePerSec int `yaml:"move_per_sec"`
	// IntentPerSec caps all other gameplay intents per second.
	IntentPerSec int `yaml:"intent_per_sec"`
}

// Config is the typed server configuration model (spec §10).
type Config struct {
	// PGDSN is the PostgreSQL connection string (VOX_PG_DSN).
	PGDSN string `yaml:"pg_dsn"`
	// WSBind is the WebSocket listen address, e.g. ":8080" (VOX_WS_BIND).
	WSBind string `yaml:"ws_bind"`
	// WorldConstantsPath locates world.toml / generated constants (VOX_WORLD_CONSTANTS_PATH).
	WorldConstantsPath string `yaml:"world_constants_path"`
	// TickHz is the simulation tick rate (VOX_TICK_HZ).
	TickHz int `yaml:"tick_hz"`
	// SnapshotIntervalSeconds is the dirty-entity saver period (VOX_SNAPSHOT_INTERVAL_SECONDS).
	SnapshotIntervalSeconds int `yaml:"snapshot_interval_seconds"`
	// RateLimits caps inbound client traffic (VOX_RATE_*).
	RateLimits RateLimitConfig `yaml:"rate_limits"`
	// SeedDataDir locates versioned seed files for `voxilian seed` (VOX_SEED_DATA_DIR).
	SeedDataDir string `yaml:"seed_data_dir"`
	// LogLevel is one of debug|info|warn|error (VOX_LOG_LEVEL).
	LogLevel string `yaml:"log_level"`
}

// DefaultPath is probed when the caller passes an empty path.
const DefaultPath = "config.yaml"

// Defaults returns the base configuration. Every field is usable as-is.
func Defaults() Config {
	return Config{
		PGDSN:                   "postgres://vox:voxdev@localhost:5432/voxilian?sslmode=disable",
		WSBind:                  ":8080",
		WorldConstantsPath:      "world.toml",
		TickHz:                  20,
		SnapshotIntervalSeconds: 60,
		RateLimits:              RateLimitConfig{MovePerSec: 10, IntentPerSec: 10},
		SeedDataDir:             "seed",
		LogLevel:                "info",
	}
}

// fileRateLimitConfig mirrors RateLimitConfig field-wise so omitted
// nested keys retain defaults instead of zeroing the whole struct.
type fileRateLimitConfig struct {
	MovePerSec   *int `yaml:"move_per_sec"`
	IntentPerSec *int `yaml:"intent_per_sec"`
}

// fileConfig mirrors Config with pointer fields so YAML overlay can
// distinguish "absent" from "explicit zero".
type fileConfig struct {
	PGDSN                   *string              `yaml:"pg_dsn"`
	WSBind                  *string              `yaml:"ws_bind"`
	WorldConstantsPath      *string              `yaml:"world_constants_path"`
	TickHz                  *int                 `yaml:"tick_hz"`
	SnapshotIntervalSeconds *int                 `yaml:"snapshot_interval_seconds"`
	RateLimits              *fileRateLimitConfig `yaml:"rate_limits"`
	SeedDataDir             *string              `yaml:"seed_data_dir"`
	LogLevel                *string              `yaml:"log_level"`
}

// Load builds the effective configuration: defaults, then the YAML file
// at path (or DefaultPath when path is ""), then VOX_* environment.
// An explicit path that does not exist is an error; a missing default
// path is not.
func Load(path string) (Config, error) {
	cfg := Defaults()

	explicit := path != ""
	if path == "" {
		path = DefaultPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !explicit && os.IsNotExist(err) {
			raw = nil // no file: defaults + environment only
		} else {
			return Config{}, fmt.Errorf("config: read %s: %w", path, err)
		}
	}
	if raw != nil {
		var fc fileConfig
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&fc); err != nil {
			return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
		}
		if fc.PGDSN != nil {
			cfg.PGDSN = *fc.PGDSN
		}
		if fc.WSBind != nil {
			cfg.WSBind = *fc.WSBind
		}
		if fc.WorldConstantsPath != nil {
			cfg.WorldConstantsPath = *fc.WorldConstantsPath
		}
		if fc.TickHz != nil {
			cfg.TickHz = *fc.TickHz
		}
		if fc.SnapshotIntervalSeconds != nil {
			cfg.SnapshotIntervalSeconds = *fc.SnapshotIntervalSeconds
		}
		if fc.RateLimits != nil {
			if fc.RateLimits.MovePerSec != nil {
				cfg.RateLimits.MovePerSec = *fc.RateLimits.MovePerSec
			}
			if fc.RateLimits.IntentPerSec != nil {
				cfg.RateLimits.IntentPerSec = *fc.RateLimits.IntentPerSec
			}
		}
		if fc.SeedDataDir != nil {
			cfg.SeedDataDir = *fc.SeedDataDir
		}
		if fc.LogLevel != nil {
			cfg.LogLevel = *fc.LogLevel
		}
	}

	cfg, err = applyEnv(cfg)
	if err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// lookupEnv reports whether name is set (even to empty).
func lookupEnv(name string) (string, bool) {
	return os.LookupEnv(name)
}

func applyEnv(cfg Config) (Config, error) {
	if v, ok := lookupEnv("VOX_PG_DSN"); ok {
		cfg.PGDSN = v
	}
	if v, ok := lookupEnv("VOX_WS_BIND"); ok {
		cfg.WSBind = v
	}
	if v, ok := lookupEnv("VOX_WORLD_CONSTANTS_PATH"); ok {
		cfg.WorldConstantsPath = v
	}
	if v, ok := lookupEnv("VOX_TICK_HZ"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid VOX_TICK_HZ %q: must be an integer", v)
		}
		cfg.TickHz = n
	}
	if v, ok := lookupEnv("VOX_SNAPSHOT_INTERVAL_SECONDS"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid VOX_SNAPSHOT_INTERVAL_SECONDS %q: must be an integer", v)
		}
		cfg.SnapshotIntervalSeconds = n
	}
	if v, ok := lookupEnv("VOX_RATE_MOVE_PER_SEC"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid VOX_RATE_MOVE_PER_SEC %q: must be an integer", v)
		}
		cfg.RateLimits.MovePerSec = n
	}
	if v, ok := lookupEnv("VOX_RATE_INTENT_PER_SEC"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("config: invalid VOX_RATE_INTENT_PER_SEC %q: must be an integer", v)
		}
		cfg.RateLimits.IntentPerSec = n
	}
	if v, ok := lookupEnv("VOX_SEED_DATA_DIR"); ok {
		cfg.SeedDataDir = v
	}
	if v, ok := lookupEnv("VOX_LOG_LEVEL"); ok {
		cfg.LogLevel = strings.ToLower(strings.TrimSpace(v))
	}
	return cfg, nil
}

// Validate performs structural checks only. Gameplay-semantic validation
// belongs to later milestones that own those domains.
func (c Config) Validate() error {
	if strings.TrimSpace(c.PGDSN) == "" {
		return fmt.Errorf("config: pg_dsn must not be empty")
	}
	if strings.TrimSpace(c.WSBind) == "" {
		return fmt.Errorf("config: ws_bind must not be empty")
	}
	if strings.TrimSpace(c.WorldConstantsPath) == "" {
		return fmt.Errorf("config: world_constants_path must not be empty")
	}
	if c.TickHz < 1 || c.TickHz > 120 {
		return fmt.Errorf("config: tick_hz %d out of range 1..120", c.TickHz)
	}
	if c.SnapshotIntervalSeconds < 1 {
		return fmt.Errorf("config: snapshot_interval_seconds %d must be >= 1", c.SnapshotIntervalSeconds)
	}
	if c.RateLimits.MovePerSec < 1 {
		return fmt.Errorf("config: rate_limits.move_per_sec %d must be >= 1", c.RateLimits.MovePerSec)
	}
	if c.RateLimits.IntentPerSec < 1 {
		return fmt.Errorf("config: rate_limits.intent_per_sec %d must be >= 1", c.RateLimits.IntentPerSec)
	}
	if strings.TrimSpace(c.SeedDataDir) == "" {
		return fmt.Errorf("config: seed_data_dir must not be empty")
	}
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("config: log_level %q must be one of debug|info|warn|error", c.LogLevel)
	}
	return nil
}
