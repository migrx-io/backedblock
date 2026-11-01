package deploy

import (
	"slices"
	"testing"
)

func TestInstanceIDs(t *testing.T) {
	// The modules output a map keyed by node; JSON gives it as map[string]any.
	m := map[string]any{"instance_ids": map[string]any{
		"pool1-10": "i-10", "pool1-2": "i-2", "pool1-0": "i-0", "pool1-1": "i-1",
	}}
	if got, want := InstanceIDs(m), []string{"i-0", "i-1", "i-2", "i-10"}; !slices.Equal(got, want) {
		t.Errorf("map: got %v, want %v", got, want)
	}
	l := map[string]any{"instance_ids": []any{"i-b", "i-a"}}
	if got, want := InstanceIDs(l), []string{"i-b", "i-a"}; !slices.Equal(got, want) {
		t.Errorf("list: got %v, want %v", got, want)
	}
	if got := InstanceIDs(map[string]any{}); len(got) != 0 {
		t.Errorf("none: got %v", got)
	}
}
