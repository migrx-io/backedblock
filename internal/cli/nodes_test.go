package cli

import "testing"

func TestStackFlag(t *testing.T) {
	for id, want := range map[string]string{
		"mgmt":       "--mgmt",
		"pool/pool1": "--pool pool1",
		"pool/io2-a": "--pool io2-a",
	} {
		if got := stackFlag(id); got != want {
			t.Errorf("stackFlag(%q) = %q, want %q", id, got, want)
		}
	}
}
