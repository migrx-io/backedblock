package cli

import (
	"maps"
	"strings"
	"testing"
)

func TestStorageClassCommand(t *testing.T) {
	params := maps.Clone(defaultQoS)
	maps.Copy(params, map[string]string{
		"qos_rw_ios_per_sec": "6000", "max_cache_r_cache_size": "476837",
		"ratio_cache_r_cache_size": "0.1", "nbd_threads": "32",
	})
	sc := &storageClassInfo{
		Bin: "./bbctl", Storage: "prod", Cluster: "main", Pool: "pool2",
		Nodes:  []string{"10.0.0.1:8082", "10.0.0.2:8082"},
		Params: params,
	}
	cmd := sc.command()
	for _, want := range []string{
		// The install runs only if reading the password worked.
		`MGX_PASSWORD="$(./bbctl secrets get prod MGX_GW_ADMIN_PASSWD)" && helm upgrade --install mgx-csi-driver oci://docker.io/migrx/mgx-csi-driver`,
		"--set csiSecret.clusterConfig.nodes='{10.0.0.1:8082,10.0.0.2:8082}'",
		"--set csiSecret.clusterConfig.username=admin",
		"--set externalSnapshotter.enabled=true",
		`--set csiSecret.clusterConfig.password="$MGX_PASSWORD"`,
		"--set storageclass.maxCacheRCacheSize=476837",
		"--set storageclass.ratioCacheRCacheSize=0.1",
		"--set storageclass.qosRMBPerSec=125", // the default
		"--set storageclass.qosRWIOPS=6000",   // the pool's
		"--set storageclass.extraParams.nbd_threads=32",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q:\n%s", want, cmd)
		}
	}
	if strings.Contains(cmd, "qosRWIOPS=3000") {
		t.Errorf("default overrode the pool's QoS:\n%s", cmd)
	}

	// A storage_class labels value goes through, its commas escaped.
	sc.Params = map[string]string{"labels": "tier=fast,env=dev"}
	if c := sc.command(); !strings.Contains(c, `--set 'storageclass.labels=tier=fast\,env=dev'`) {
		t.Errorf("labels: %s", c)
	}
	sc.Params = nil
	if c := sc.command(); strings.Contains(c, "storageclass") {
		t.Errorf("storageclass values without parameters: %s", c)
	}
}

func TestStorageClassMissingNote(t *testing.T) {
	sc := &storageClassInfo{Storage: "dev", Pool: "pool1"}
	if n := sc.missingNote(); !strings.Contains(n, "pool1 has no storage_class in dev.bbctl.yaml") {
		t.Errorf("note = %q", n)
	}
	sc.Configured = true
	if n := sc.missingNote(); n != "" {
		t.Errorf("configured pool got a note: %q", n)
	}
}
