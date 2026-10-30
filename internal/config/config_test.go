package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func valid() *Config {
	return &Config{
		Name: "bb", Region: "us-east-1", Cluster: "main",
		State: State{Bucket: "bb-tfstate"}, Modules: Modules{Ref: "main"},
		NodesAMI: "ami-1", SecretsSSMPath: "/mgx/bb/secrets",
		Network: Network{
			VPCID: "vpc-1", AZs: []string{"us-east-1a"},
			MgmtSubnetCIDRs: []string{"10.0.0.0/20"}, StorageSubnetCIDRs: []string{"10.0.16.0/20"},
			NATPublicSubnetID: "subnet-1",
		},
		Pools: []Pool{DefaultPool("bb", "123456789012", "pool1", "us-east-1a")},
	}
}

func TestValidate(t *testing.T) {
	if errs := valid().Validate(); len(errs) > 0 {
		t.Fatalf("valid config rejected: %v", errs)
	}

	c := valid()
	c.Pools = append(c.Pools, DefaultPool("bb", "123456789012", "pool2", "us-east-1b"))
	c.Pools[1].S3BucketNames = c.Pools[0].S3BucketNames
	errs := strings.Join(c.Validate(), "\n")
	// Independent pools without a management plane are a valid layout.
	if strings.Contains(errs, "management plane") {
		t.Errorf("two pools without mgmt must be valid:\n%s", errs)
	}
	for _, want := range []string{"not one of network.azs", "already used by pool"} {
		if !strings.Contains(errs, want) {
			t.Errorf("missing %q in:\n%s", want, errs)
		}
	}
}

func TestSaveLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bbctl.yaml")
	c := valid()
	c.Pools[0].Extra = map[string]any{"cache_filesystem": "xfs"}
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Pools[0].Extra["cache_filesystem"] != "xfs" || got.Pools[0].DiskCount() != 10 {
		t.Fatalf("round trip lost data: %+v", got.Pools[0])
	}
}

func TestStageCommit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bbctl.yaml")
	c := valid()
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}

	next := *c
	next.Pools = nil
	st, err := next.Stage(p)
	if err != nil {
		t.Fatal(err)
	}
	// Until Commit, the old config is still in place.
	if got, _ := Load(p); len(got.Pools) != 1 {
		t.Fatal("Stage replaced the config before Commit")
	}
	if err := st.Commit(); err != nil {
		t.Fatal(err)
	}
	st.Discard()
	if got, _ := Load(p); len(got.Pools) != 0 {
		t.Fatal("Commit did not replace the config")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*.tmp")); len(left) > 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	// Discard without Commit leaves the original untouched.
	st, err = c.Stage(p)
	if err != nil {
		t.Fatal(err)
	}
	st.Discard()
	if got, _ := Load(p); len(got.Pools) != 0 {
		t.Fatal("Discard changed the config")
	}
}

func TestStageUnwritable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := valid().Stage(filepath.Join(dir, "bbctl.yaml")); err == nil {
		t.Fatal("Stage into a read-only dir should fail before anything irreversible happens")
	}
}
