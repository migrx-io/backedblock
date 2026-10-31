package cli

import (
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

func TestResolveConfig(t *testing.T) {
	resolve := func(storage string) (string, error) {
		a := &app{storage: storage}
		err := a.resolveConfig()
		return a.configPath, err
	}
	if got, err := resolve("dev"); err != nil || got != "dev.bbctl.yaml" {
		t.Errorf("NAME dev: %q, %v", got, err)
	}
	if _, err := resolve("../x"); err == nil {
		t.Error("NAME ../x: want an error")
	}
}
