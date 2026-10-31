package tf

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/migrx-io/backedblock/internal/ui"
)

// ModulesRepo is the GitHub repository holding the Terraform modules.
const ModulesRepo = "migrx-io/terraform-aws-mgx"

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// EnsureModules returns a local directory containing the terraform-aws-mgx
// repo at ref. It is fetched over HTTPS as a tarball (no git needed) and
// cached by commit SHA, so a moving ref like "main" is re-resolved each run
// while an unchanged commit is never downloaded twice. localPath short-cuts
// all of it for module development.
func EnsureModules(ctx context.Context, ref, localPath string) (dir, sha string, err error) {
	if localPath != "" {
		abs, err := filepath.Abs(localPath)
		if err != nil {
			return "", "", err
		}
		if _, err := os.Stat(filepath.Join(abs, "modules", "pool")); err != nil {
			return "", "", fmt.Errorf("modules.path %s: no modules/pool inside", abs)
		}
		return abs, "local", nil
	}
	cache, err := CacheDir()
	if err != nil {
		return "", "", err
	}
	refFile := refPath(cache, ref)

	sha, err = resolveRef(ctx, ref)
	if err != nil {
		// Offline or rate-limited: fall back to the last resolution of this ref.
		b, rerr := os.ReadFile(refFile)
		if rerr != nil {
			return "", "", fmt.Errorf("resolve %s@%s: %w", ModulesRepo, ref, err)
		}
		sha = strings.TrimSpace(string(b))
		ui.Logf("could not resolve modules ref %q (%v); using cached %s", ref, err, sha[:12])
	} else if err := os.MkdirAll(filepath.Dir(refFile), 0o755); err == nil {
		_ = os.WriteFile(refFile, []byte(sha), 0o644)
	}

	dir = filepath.Join(cache, "modules", sha)
	if _, err := os.Stat(filepath.Join(dir, "modules")); err == nil {
		return dir, sha, nil
	}
	ui.Logf("downloading %s@%s (%s)", ModulesRepo, ref, sha[:12])
	tmp := dir + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := download(ctx, fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", ModulesRepo, sha), tmp); err != nil {
		return "", "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", "", err
	}
	return dir, sha, nil
}

// CachedModulesSHA is the commit ref last resolved to, without the network:
// "" if init has not fetched it yet.
func CachedModulesSHA(ref string) string {
	cache, err := CacheDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(refPath(cache, ref))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func refPath(cache, ref string) string {
	return filepath.Join(cache, "modules", "refs", strings.ReplaceAll(ref, "/", "_"))
}

func resolveRef(ctx context.Context, ref string) (string, error) {
	if shaRe.MatchString(ref) {
		return ref, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("https://api.github.com/repos/%s/commits/%s", ModulesRepo, ref), nil)
	req.Header.Set("Accept", "application/vnd.github.sha")
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github: %s", resp.Status)
	}
	sha := strings.TrimSpace(string(b))
	if !shaRe.MatchString(sha) {
		return "", fmt.Errorf("github: unexpected response %q", sha)
	}
	return sha, nil
}

// download extracts a GitHub tarball into dst, dropping its top-level dir.
func download(ctx context.Context, url, dst string) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// Strip "<repo>-<sha>/".
		_, rel, ok := strings.Cut(h.Name, "/")
		if !ok || rel == "" {
			continue
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) {
			return fmt.Errorf("tarball entry escapes destination: %s", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o644)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
}
