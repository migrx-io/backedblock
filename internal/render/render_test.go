package render

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/migrx-io/backedblock/internal/config"
)

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	mods := filepath.Join(dir, "cache", "modules")
	c := &config.Config{
		Name: "bb", Region: "us-east-1", Cluster: "main",
		State: config.State{Bucket: "bb-tfstate"}, NodesAMI: "ami-1", SecretsSSMPath: "/mgx/bb/secrets",
		Network: config.Network{VPCID: "vpc-1", AZs: []string{"us-east-1a"}},
		Mgmt:    config.DefaultMgmt(),
		Pools:   []config.Pool{config.DefaultPool("bb", "123456789012", "pool1", "us-east-1a")},
	}
	c.Pools[0].Extra = map[string]any{"scheduler_node_down_wait": 60}
	st := Stacks(c, filepath.Join(dir, "work"))
	if len(st) != 3 || st[0].ID != "network" || st[1].ID != "pool/pool1" || st[2].ID != "mgmt" {
		t.Fatalf("stack order %+v", st)
	}
	if err := Write(c, st, Inputs{ModulesDir: filepath.Dir(mods), SSHPublicKeyPath: "/k.pub"}); err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	b, err := os.ReadFile(filepath.Join(st[1].Dir, "main.tf.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	pool := doc["module"].(map[string]any)["pool"].(map[string]any)
	if src := pool["source"].(string); src != "../../../../cache/modules/pool" {
		t.Errorf("source = %s", src)
	}
	if pool["cross_peer_scrape"] != false {
		t.Errorf("pool under mgmt should not cross-scrape")
	}
	if pool["scheduler_node_down_wait"] != float64(60) {
		t.Errorf("extra not passed through: %v", pool["scheduler_node_down_wait"])
	}
	if pool["nvme_node_disks_count"] != float64(10) {
		t.Errorf("nvme_node_disks_count = %v", pool["nvme_node_disks_count"])
	}
	backend := doc["terraform"].(map[string]any)["backend"].(map[string]any)["s3"].(map[string]any)
	if backend["key"] != "bb/pools/pool1.tfstate" {
		t.Errorf("state key = %v", backend["key"])
	}
}

func TestWriteLocal(t *testing.T) {
	dir := t.TempDir()
	c := &config.Config{
		Name: "bb", Region: "us-east-1", Cluster: "main",
		State:   config.State{Bucket: "bb-tfstate"},
		Network: config.Network{VPCID: "vpc-1", AZs: []string{"us-east-1a"}},
	}
	st := Stacks(c, filepath.Join(dir, "work"))
	preview := filepath.Join(dir, "work", "preview", "network")
	if err := WriteLocal(c, st[0], st, Inputs{ModulesDir: filepath.Join(dir, "mods")}, preview); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	b, _ := os.ReadFile(filepath.Join(preview, "main.tf.json"))
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	backend := doc["terraform"].(map[string]any)["backend"].(map[string]any)
	if _, ok := backend["s3"]; ok || backend["local"] == nil {
		t.Fatalf("preview must use a local backend, got %v", backend)
	}
	if src := doc["module"].(map[string]any)["network"].(map[string]any)["source"]; src != "../../../mods/modules/network" {
		t.Errorf("source relative to the preview dir = %v", src)
	}
	// The real stack is untouched.
	if _, err := os.Stat(filepath.Join(st[0].Dir, "main.tf.json")); err == nil {
		t.Error("WriteLocal wrote into the real stack dir")
	}
}
