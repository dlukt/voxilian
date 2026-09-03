package simtest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// RepoRoot returns the repository root directory (the Godot project
// root containing project.godot and backend/voxilian/go.mod).
//
// Resolution is anchored at this source file's location, not the process
// working directory, so it works from package dirs, `go test ./...`,
// and CI. No hard-coded absolute paths.
func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("simtest: runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		godot := filepath.Join(dir, "project.godot")
		gomod := filepath.Join(dir, "backend", "voxilian", "go.mod")
		if fileExists(godot) && fileExists(gomod) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("simtest: repository root not found above %s", filepath.Dir(file))
		}
		dir = parent
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// ProtocolGoldenPath returns the path of a named fixture under repo-root
// testdata/protocol/. Underscore-prefixed names are harness sentinels,
// never real protocol vectors (M2 excludes them from discovery).
func ProtocolGoldenPath(t *testing.T, name string) string {
	t.Helper()
	if strings.ContainsRune(name, '/') || strings.Contains(name, "..") {
		t.Fatalf("simtest: invalid fixture name %q", name)
	}
	return filepath.Join(RepoRoot(t), "testdata", "protocol", name)
}

// decodeHex is the pure core: hex with ordinary whitespace/newlines
// tolerated, malformed input reported as an error.
func decodeHex(name, text string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			return -1
		default:
			return r
		}
	}, text)
	if len(clean)%2 != 0 {
		return nil, fmt.Errorf("simtest: fixture %s has odd hex length %d", name, len(clean))
	}
	out := make([]byte, len(clean)/2)
	for i := range out {
		hi, ok1 := hexVal(clean[2*i])
		lo, ok2 := hexVal(clean[2*i+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("simtest: fixture %s has invalid hex byte %q at offset %d", name, clean[2*i:2*i+2], i)
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

// DecodeHexBytes decodes hex with ordinary whitespace/newlines tolerated.
// It reports malformed input as an error for negative tests.
func DecodeHexBytes(t *testing.T, name, text string) []byte {
	t.Helper()
	out, err := decodeHex(name, text)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// ProtocolGolden reads the named .hex fixture and returns its bytes.
func ProtocolGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(ProtocolGoldenPath(t, name))
	if err != nil {
		t.Fatalf("simtest: read fixture %s: %v", name, err)
	}
	return DecodeHexBytes(t, name, string(raw))
}
