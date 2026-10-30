// Package tf provisions the toolchain bbctl drives (a pinned Terraform binary
// and the terraform-aws-mgx modules) and runs Terraform on rendered stacks.
package tf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"

	"github.com/migrx-io/backedblock/internal/ui"
)

// DefaultVersion is the Terraform release bbctl downloads when the config does
// not pin one. Needs >= 1.10 for S3-native state locking (use_lockfile).
const DefaultVersion = "1.16.7"

// CacheDir is where bbctl keeps downloaded tools, modules and providers.
func CacheDir() (string, error) {
	if d := os.Getenv("BBCTL_CACHE_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "bbctl"), nil
}

// EnsureTerraform returns the path to a terraform binary, downloading the
// pinned version into the cache on first use. Precedence: BBCTL_TERRAFORM env,
// explicit path, cached download.
func EnsureTerraform(ctx context.Context, explicitPath, ver string) (string, error) {
	if p := os.Getenv("BBCTL_TERRAFORM"); p != "" {
		return p, nil
	}
	if explicitPath != "" {
		return explicitPath, nil
	}
	if ver == "" {
		ver = DefaultVersion
	}
	v, err := version.NewVersion(ver)
	if err != nil {
		return "", fmt.Errorf("terraform version %q: %w", ver, err)
	}
	cache, err := CacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "terraform", v.String())
	bin := filepath.Join(dir, "terraform")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ui.Logf("downloading terraform %s", v)
	inst := &releases.ExactVersion{Product: product.Terraform, Version: v, InstallDir: dir}
	return inst.Install(ctx)
}

// TerraformVersion reports the version of the binary at path.
func TerraformVersion(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "version", "-json").Output()
	if err != nil {
		return "", err
	}
	var v struct {
		TerraformVersion string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	return v.TerraformVersion, nil
}
