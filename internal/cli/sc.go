package cli

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/deploy"
	"github.com/migrx-io/backedblock/internal/render"
	"github.com/migrx-io/backedblock/internal/ui"
)

// scPasswordKey is the node secret's key for the API's admin password.
const scPasswordKey = "MGX_GW_ADMIN_PASSWD"

func (a *app) scCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sc",
		Short: "print how to install the CSI driver for this storage",
		Long: `Sc prints the helm command that installs the mgx CSI driver for this storage:
the API nodes it connects to, the cluster, the admin login, and the
StorageClass's cache sizes and QoS caps from the pool's storage_class.`,
		Example: `  bbctl sc prod`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			sc, err := a.storageClass(ctx, d)
			if err != nil {
				return err
			}
			res := map[string]any{
				"storage": sc.Storage, "pool": sc.Pool, "nodes": sc.Nodes,
				"command": sc.command(), "docs": []string{docsInstall, docsStorageClass},
			}
			return ui.Result(res, func(w io.Writer) {
				fmt.Fprintf(w, "Install the CSI driver (%s):\n\n%s\n\n", docsInstall, sc.command())
				fmt.Fprintf(w, "More details about storage classes: %s\n", docsStorageClass)
			})
		},
	}
	return cmd
}

const (
	docsInstall      = "https://backedblock.io/docs/quickstart"
	docsStorageClass = "https://backedblock.io/docs/storage-class"
)

// defaultQoS are the caps a pool's storage_class may leave out: what the
// calculator sizes a volume for by default.
var defaultQoS = map[string]string{
	"qos_r_mbytes_per_sec":  "125",
	"qos_w_mbytes_per_sec":  "125",
	"qos_rw_mbytes_per_sec": "125",
	"qos_rw_ios_per_sec":    "3000",
}

// storageClassInfo is what the driver needs to reach the storage.
type storageClassInfo struct {
	// Bin is how to call this bbctl in the printed command.
	Bin     string
	Storage string
	Cluster string
	Pool    string
	// Nodes are the API endpoints, <ip>:8082: the mgmt nodes, or the pool's.
	Nodes []string
	// Params are the pool's storage_class parameters, as the driver names
	// them, with defaultQoS for the caps it leaves out.
	Params map[string]string
}

// storageClass reads what the driver needs: the API is the management
// plane's when there is one, which reaches every pool; else the first pool's.
func (a *app) storageClass(ctx context.Context, d *deploy.Deployment) (*storageClassInfo, error) {
	sc := &storageClassInfo{Bin: selfCommand(), Storage: d.Cfg.Name, Cluster: d.Cfg.Cluster}
	var poolStack, mgmtStack *render.Stack
	for i, s := range d.Stacks {
		switch s.Kind {
		case render.KindPool:
			if poolStack == nil {
				poolStack = &d.Stacks[i]
			}
		case render.KindMgmt:
			mgmtStack = &d.Stacks[i]
		}
	}
	if poolStack == nil {
		return nil, ui.Errf(ui.ExitUsage, "add one with `bbctl pool add "+d.Cfg.Name+" pool1`", "the config has no pools")
	}
	sc.Pool = poolStack.Pool
	sc.Params = maps.Clone(defaultQoS)
	for _, p := range d.Cfg.Pools {
		if p.Name == sc.Pool {
			maps.Copy(sc.Params, p.StorageClass)
		}
	}

	api := poolStack
	if mgmtStack != nil {
		api = mgmtStack
	}
	nodes, err := a.stackNodes(ctx, d, *api)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.State == "running" && n.PrivateIP != "" {
			sc.Nodes = append(sc.Nodes, n.PrivateIP+":8082")
		}
	}
	if len(sc.Nodes) == 0 {
		return nil, ui.Errf(ui.ExitAWS, "check `bbctl status "+d.Cfg.Name+"`", "%s has no running nodes", api.ID)
	}
	return sc, nil
}

// chartValues maps StorageClass parameters to the chart's storageclass.*
// values; any other parameter goes through storageclass.extraParams.
var chartValues = []struct{ param, value string }{
	{"config", "config"},
	{"labels", "labels"},
	{"min_cache_r_cache_size", "minCacheRCacheSize"},
	{"min_cache_rw_cache_size", "minCacheRWCacheSize"},
	{"max_cache_r_cache_size", "maxCacheRCacheSize"},
	{"max_cache_rw_cache_size", "maxCacheRWCacheSize"},
	{"ratio_cache_r_cache_size", "ratioCacheRCacheSize"},
	{"ratio_cache_rw_cache_size", "ratioCacheRWCacheSize"},
	{"qos_r_mbytes_per_sec", "qosRMBPerSec"},
	{"qos_w_mbytes_per_sec", "qosWMBPerSec"},
	{"qos_rw_mbytes_per_sec", "qosRWMBPerSec"},
	{"qos_rw_ios_per_sec", "qosRWIOPS"},
	{"storage_encrypt_secret", "storageEncryptSecret"},
	{"storage_compress", "storageCompress"},
}

// storageClassValues are the chart's storageclass.* keys and values, in the
// chart's order, extraParams last.
func (sc *storageClassInfo) storageClassValues() [][2]string {
	params := maps.Clone(sc.Params)
	var out [][2]string
	for _, c := range chartValues {
		if v, ok := params[c.param]; ok {
			out = append(out, [2]string{c.value, v})
			delete(params, c.param)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(params)) {
		out = append(out, [2]string{"extraParams." + k, params[k]})
	}
	return out
}

// selfCommand is how the user called bbctl: the same path when it was run as
// ./bbctl, so the printed command does not pick up another bbctl on PATH.
func selfCommand() string {
	if strings.ContainsRune(os.Args[0], os.PathSeparator) {
		return shellQuote(os.Args[0])
	}
	return "bbctl"
}

// command is the helm install, one flag per line. The password is read first
// and the install runs only if that worked: a failed command substitution
// inside the helm line would install the driver with an empty password.
func (sc *storageClassInfo) command() string {
	lines := []string{
		fmt.Sprintf(`MGX_PASSWORD="$(%s secrets get %s %s)" && helm upgrade --install mgx-csi-driver oci://docker.io/migrx/mgx-csi-driver`, sc.Bin, sc.Storage, scPasswordKey),
		"--namespace mgx-system --create-namespace",
		"--set csiSecret.clusterConfig.nodes='{" + strings.Join(sc.Nodes, ",") + "}'",
		"--set csiSecret.clusterConfig.cluster=" + sc.Cluster,
		"--set csiSecret.clusterConfig.ns=" + sc.Cluster,
		"--set csiSecret.clusterConfig.username=admin",
		`--set csiSecret.clusterConfig.password="$MGX_PASSWORD"`,
	}
	lines = append(lines, "--set externalSnapshotter.enabled=true")
	for _, kv := range sc.storageClassValues() {
		// helm splits --set on commas; a value's own are escaped.
		lines = append(lines, "--set "+shellQuote("storageclass."+kv[0]+"="+strings.ReplaceAll(kv[1], ",", `\,`)))
	}
	return strings.Join(lines, " \\\n  ")
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
