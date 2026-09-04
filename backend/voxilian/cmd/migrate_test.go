package cmd

import "testing"

// TestMigrateCommandTree proves the migrate hierarchy exists with the
// M1-T8 leaf commands (behavior is covered by store migrator tests and
// the hosted binary run).
func TestMigrateCommandTree(t *testing.T) {
	root := newMigrateCmd()
	want := map[string]bool{"up": false, "down": false, "status": false}
	for _, sub := range root.Commands() {
		if _, ok := want[sub.Name()]; !ok {
			t.Fatalf("unexpected subcommand %q", sub.Name())
		}
		want[sub.Name()] = true
	}
	for name, seen := range want {
		if !seen {
			t.Fatalf("missing subcommand %q", name)
		}
	}
}
