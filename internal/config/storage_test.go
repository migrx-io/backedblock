package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWorkDir(t *testing.T) {
	for in, want := range map[string]string{
		"bbctl.yaml":                       ".bbctl",
		"dir/bbctl.yaml":                   "dir/.bbctl",
		"prod.bbctl.yaml":                  ".bbctl/prod",
		"dir/dev.bbctl.yaml":               "dir/.bbctl/dev",
		"other.yaml":                       ".bbctl",
		filepath.Join(".", "x.bbctl.yaml"): ".bbctl/x",
	} {
		if got := WorkDir(in); got != filepath.FromSlash(want) {
			t.Errorf("WorkDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStorages(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"prod.bbctl.yaml", "dev.bbctl.yaml", "bbctl.yaml", "notes.yaml"} {
		os.WriteFile(filepath.Join(dir, f), nil, 0o644)
	}
	os.Mkdir(filepath.Join(dir, "dir.bbctl.yaml"), 0o755)
	got, err := Storages(dir)
	if err != nil || !reflect.DeepEqual(got, []string{"dev", "prod"}) {
		t.Fatalf("Storages = %v, %v", got, err)
	}
}
