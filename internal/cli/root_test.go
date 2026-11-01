package cli

import (
	"slices"
	"testing"
)

// The command set is part of the agent contract (AGENTS.md); removed commands
// must not come back unlisted.
func TestCommands(t *testing.T) {
	var got []string
	for _, c := range NewRoot().Commands() {
		if c.Hidden {
			t.Errorf("%s is hidden: delete it or list it", c.Name())
		}
		got = append(got, c.Name())
	}
	want := []string{"apply", "config", "connect", "destroy", "exec", "help", "init", "plan", "pool", "secrets", "status", "version"}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("commands = %v, want %v", got, want)
	}
}
