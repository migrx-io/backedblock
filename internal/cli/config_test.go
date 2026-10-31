package cli

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/migrx-io/backedblock/internal/config"
)

// What /api/estimate returns as config.pool: comments and all.
const calculatorPool = `# bbctl.yaml - the sized fields of one entry under pools:; name, az
# and buckets stay as bbctl init / bbctl pool add wrote them (see /docs/storage-cluster).

nodes_count: 15
instance_type: c8gb.4xlarge   # 16 vCPU, 32 GiB

raid_level: 0
max_volumes_count: 5

r_cache_size_in_mib: 437737   # per disk; the volumes' whole footprint
rw_cache_size_in_mib: 131321  # per disk; dirty data inside it

ebs_volumes:                  # 4 disks per node, striped
  - size: 460
    type: gp3
    iops: 3000                # 30,000 IOPS across the stripe, on the free baseline
    throughput: 125           # 1,250 MB/s across the stripe, on the free baseline
    count: 4
`

func TestCalculatorPoolDecodesOverDefaults(t *testing.T) {
	p := config.DefaultPool("prod", "123456789012", "pool1", "")
	if err := yaml.Unmarshal([]byte(calculatorPool), &p); err != nil {
		t.Fatal(err)
	}
	if p.NodesCount != 15 || p.InstanceType != "c8gb.4xlarge" || p.MaxVolumesCount != 5 ||
		p.RCacheSizeInMiB != 437737 || p.RWCacheSizeInMiB != 131321 {
		t.Errorf("sizing not applied: %+v", p)
	}
	want := []config.EBSVolume{{Size: 460, Type: "gp3", IOPS: 3000, Throughput: 125, Count: 4}}
	if len(p.EBSVolumes) != 1 || p.EBSVolumes[0] != want[0] {
		t.Errorf("ebs_volumes = %+v, want %+v (replaced, not merged)", p.EBSVolumes, want)
	}
	if len(p.S3BucketNames) == 0 {
		t.Error("defaults the calculator does not set were lost")
	}
}

func TestUSD(t *testing.T) {
	for in, want := range map[float64]string{0: "$0", 999.4: "$999", 12331.01: "$12,331", 1720000: "$1,720,000"} {
		if got := usd(in); got != want {
			t.Errorf("usd(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestResolveConfig(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	os.Chdir(dir)

	resolve := func(storage, path string) (string, error) {
		a := &app{storage: storage, configPath: path}
		err := a.resolveConfig()
		return a.configPath, err
	}
	if got, err := resolve("", ""); err != nil || got != "bbctl.yaml" {
		t.Errorf("empty dir: %q, %v", got, err)
	}
	os.WriteFile("prod.bbctl.yaml", nil, 0o644)
	if got, err := resolve("", ""); err != nil || got != "prod.bbctl.yaml" {
		t.Errorf("one storage: %q, %v", got, err)
	}
	os.WriteFile("bbctl.yaml", nil, 0o644)
	if _, err := resolve("", ""); err == nil {
		t.Error("storage plus bbctl.yaml: want an error")
	}
	if got, err := resolve("dev", ""); err != nil || got != "dev.bbctl.yaml" {
		t.Errorf("--storage dev: %q, %v", got, err)
	}
	if _, err := resolve("../x", ""); err == nil {
		t.Error("--storage ../x: want an error")
	}
	if _, err := resolve("dev", "a.yaml"); err == nil {
		t.Error("--storage and --config: want an error")
	}
	if got, err := resolve("", "a.yaml"); err != nil || got != "a.yaml" {
		t.Errorf("--config: %q, %v", got, err)
	}
}
