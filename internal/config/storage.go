package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// StorageSuffix ends every named storage config: `bbctl config create prod`
// writes prod.bbctl.yaml.
const StorageSuffix = ".bbctl.yaml"

// WorkDirName holds generated stacks and keys, next to the config.
const WorkDirName = ".bbctl"

// StorageFile is the config file of a named storage in dir.
func StorageFile(dir, name string) string {
	return filepath.Join(dir, name+StorageSuffix)
}

// Storages lists the named storage configs in dir, by name.
func Storages(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*"+StorageSuffix))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, f := range files {
		if st, err := os.Stat(f); err == nil && st.Mode().IsRegular() {
			names = append(names, strings.TrimSuffix(filepath.Base(f), StorageSuffix))
		}
	}
	sort.Strings(names)
	return names, nil
}

// WorkDir is where a config's stacks and SSH key live: .bbctl/<name> for a
// named storage, so several share a directory without clobbering each other,
// and .bbctl itself for a plain bbctl.yaml, which is where it always was. The
// key in it is registered with EC2, so moving it would replace the key pair.
func WorkDir(configPath string) string {
	dir, base := filepath.Dir(configPath), filepath.Base(configPath)
	if name, ok := strings.CutSuffix(base, StorageSuffix); ok && name != "" {
		return filepath.Join(dir, WorkDirName, name)
	}
	return filepath.Join(dir, WorkDirName)
}

// ValidName reports whether s can name a deployment (and so a storage).
func ValidName(s string) bool { return nameRe.MatchString(s) }
