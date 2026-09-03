package simtest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRepoRootResolution(t *testing.T) {
	root := RepoRoot(t)
	for _, want := range []string{"project.godot", "backend/voxilian/go.mod", "testdata/protocol/_harness_example.hex"} {
		if _, err := os.Stat(filepath.Join(root, want)); err != nil {
			t.Fatalf("root %s missing %s: %v", root, want, err)
		}
	}
}

func TestSentinelGolden(t *testing.T) {
	got := ProtocolGolden(t, "_harness_example.hex")
	want := []byte{0x00, 0x01, 0x7f, 0xff}
	if !bytes.Equal(got, want) {
		t.Fatalf("decoded = % x, want % x", got, want)
	}
}

func TestGoldenMissingFails(t *testing.T) {
	path := ProtocolGoldenPath(t, "_does_not_exist.hex")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected missing fixture at %s", path)
	}
}

// Malformed hex is rejected through the real decode core (pure,
// error-returning) without touching repo fixtures or killing the binary.
func TestDecodeHexBytesNegative(t *testing.T) {
	for _, tc := range []string{"0", "zz", "12 3x", "ab\ncd\n1", "0x12"} {
		if _, err := decodeHex("neg", tc); err == nil {
			t.Errorf("decodeHex(%q) accepted malformed input", tc)
		}
	}
	if _, err := decodeHex("ok", "00 01\n7F fF\r\n"); err != nil {
		t.Errorf("decodeHex valid input: %v", err)
	}
}
