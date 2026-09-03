package observe

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// ParseLevel maps configured log-level strings to slog levels.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("observe: log level %q must be one of debug|info|warn|error", s)
	}
}

// NewLogger builds a JSON structured logger at the given level writing
// to w. JSON is the server-operation default; callers own the writer
// (usually os.Stderr). No package-global mutable logger state.
func NewLogger(w io.Writer, level string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}

// WithTick attaches the canonical sim-tick field (spec §10).
func WithTick(l *slog.Logger, tick uint32) *slog.Logger {
	return l.With("tick", tick)
}

// WithCell attaches the canonical map-cell fields (spec §10).
func WithCell(l *slog.Logger, cx, cz int32) *slog.Logger {
	return l.With("cell", map[string]int32{"cx": cx, "cz": cz})
}

// WithCharID attaches the canonical character field (spec §10).
func WithCharID(l *slog.Logger, charID int64) *slog.Logger {
	return l.With("charID", charID)
}
