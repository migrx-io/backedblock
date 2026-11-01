package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/config"
	"github.com/migrx-io/backedblock/internal/deploy"
	"github.com/migrx-io/backedblock/internal/ui"
)

func (a *app) poolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "add, list or remove the storage pools in a config",
	}
	cmd.AddCommand(a.poolAddCmd(), a.poolRemoveCmd(), a.poolListCmd())
	return cmd
}

func (a *app) poolAddCmd() *cobra.Command {
	var az, instanceType string
	var nodes int
	var withMgmt bool
	cmd := &cobra.Command{
		Use:   "add POOL",
		Short: "add a pool to NAME.bbctl.yaml (then run bbctl apply NAME)",
		Long:  `Add appends a pool with the default sizing.`,
		Example: `  bbctl pool add prod pool2 --az us-east-1b                     # an independent pool
  bbctl pool add prod pool2 --with-mgmt && bbctl apply prod   # pools behind a management plane`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.load()
			if err != nil {
				return err
			}
			name := args[0]
			if _, ok := c.Pool(name); ok {
				return ui.Errf(ui.ExitUsage, "", "pool %q already exists", name)
			}
			mgmtAdded := withMgmt && c.Mgmt == nil
			if mgmtAdded {
				c.Mgmt = config.DefaultMgmt()
				for i := range c.Pools {
					c.Pools[i].EnableGrafana = false
				}
			}
			if az == "" {
				az = c.Network.AZs[len(c.Pools)%len(c.Network.AZs)]
			}
			aws, err := awsx.New(cmd.Context(), c.Region, a.profile)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			id, err := aws.Identity(cmd.Context())
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			account := id.Account
			p := config.DefaultPool(c.Name, account, name, az)
			if instanceType != "" {
				p.InstanceType = instanceType
			}
			if nodes > 0 {
				p.NodesCount = nodes
			}
			// An independent pool runs its own Grafana; under mgmt it runs there.
			p.EnableGrafana = c.Mgmt == nil
			c.Pools = append(c.Pools, p)
			if errs := c.Validate(); len(errs) > 0 {
				e := ui.Errf(ui.ExitUsage, "", "pool %q would make the config invalid", name)
				e.Details = errs
				return e
			}
			if err := c.Save(a.configPath); err != nil {
				return err
			}
			var notes []string
			if mgmtAdded {
				notes = []string{
					fmt.Sprintf("added a management plane (%d x %s)", c.Mgmt.NodesCount, c.Mgmt.InstanceType),
					"existing pools switch to mgmt-federated metrics and Grafana moves to mgmt; their nodes re-provision on apply",
					"after apply, point the CSI driver at the mgmt nodes (outputs.node_private_ips of the mgmt stack in `bbctl status " + c.Name + " --json`)",
				}
			} else if c.Mgmt == nil {
				notes = []string{"independent pool: its own API endpoints (outputs.node_mgmt_private_ips of pool/" + p.Name + ") and its own Grafana"}
			}
			res := map[string]any{"pool": p, "mgmt": c.Mgmt != nil, "mgmt_added": mgmtAdded, "notes": notes, "next": "bbctl apply " + c.Name}
			return ui.Result(res, func(w io.Writer) {
				fmt.Fprintf(w, "added %s: %d x %s in %s\n", p.Name, p.NodesCount, p.InstanceType, p.AZ)
				for _, n := range notes {
					fmt.Fprintf(w, "  - %s\n", n)
				}
				fmt.Fprintln(w, "next: bbctl apply "+c.Name)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&az, "az", "", "AZ for the pool (one of network.azs; default: round-robin)")
	f.StringVar(&instanceType, "instance-type", "", "node instance type (default "+config.DefaultInstanceType+")")
	f.IntVar(&nodes, "nodes", 0, fmt.Sprintf("node count (default %d)", config.DefaultNodesCount))
	f.BoolVar(&withMgmt, "with-mgmt", false, "also add a management plane if there is none")
	return cmd
}

func (a *app) poolRemoveCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove POOL",
		Short: "destroy a pool and remove it from NAME.bbctl.yaml",
		Long: `Remove destroys the pool's stack (nodes, cache disks AND its buckets with
all volume data), removes it from NAME.bbctl.yaml, then re-applies mgmt so it
forgets the pool.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			name := args[0]
			if _, ok := d.Cfg.Pool(name); !ok {
				return ui.Errf(ui.ExitUsage, "", "no pool %q", name)
			}
			if len(d.Cfg.Pools) == 1 {
				return ui.Errf(ui.ExitUsage, "use `bbctl destroy "+d.Cfg.Name+"` to remove the whole deployment", "%q is the only pool", name)
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			// Write the new config before destroying anything: if it cannot be
			// written, nothing is destroyed. Afterwards only an atomic rename
			// is left, so the config cannot keep listing a destroyed pool.
			var kept []config.Pool
			for _, p := range d.Cfg.Pools {
				if p.Name != name {
					kept = append(kept, p)
				}
			}
			next := *d.Cfg
			next.Pools = kept
			staged, err := next.Stage(a.configPath)
			if err != nil {
				return ui.Wrap(ui.ExitError, "nothing was destroyed; make the config writable and re-run", err)
			}
			defer staged.Discard()

			id := "pool/" + name
			res, err := d.Destroy(ctx, map[string]bool{id: true}, approver(yes, "destroy"))
			if err != nil {
				return err
			}
			if err := staged.Commit(); err != nil {
				return ui.Wrap(ui.ExitError, fmt.Sprintf("pool %q is destroyed but still listed in %s; re-run `bbctl pool remove %s %s` (safe: nothing is left to destroy). Do not run `bbctl apply` first: it would recreate the pool", name, a.configPath, d.Cfg.Name, name), err)
			}
			d.Cfg = &next
			if d.Cfg.Mgmt != nil {
				d2, err := a.open(ctx)
				if err != nil {
					return err
				}
				if err := d2.Prepare(ctx, deploy.BucketRequire); err != nil {
					return err
				}
				more, err := d2.Apply(ctx, map[string]bool{"mgmt": true}, approver(yes, "apply"))
				res = append(res, more...)
				if err != nil {
					e := ui.Wrap(ui.ExitTerraform, "", err)
					e.Hint = fmt.Sprintf("pool %q is removed; finish with `bbctl apply %s --stack mgmt` so mgmt forgets it", name, d.Cfg.Name)
					return e
				}
			}
			return printResults(res)
		},
	}
	cmd.Flags().BoolVar(&yes, "auto-approve", false, "destroy without asking")
	return cmd
}

func (a *app) poolListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list pools in NAME.bbctl.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.load()
			if err != nil {
				return err
			}
			return ui.Result(map[string]any{"pools": c.Pools}, func(w io.Writer) {
				for _, p := range c.Pools {
					fmt.Fprintf(w, "%-16s %d x %-14s %-12s cache %d x %d GiB\n", p.Name, p.NodesCount, p.InstanceType, p.AZ, p.DiskCount(), p.EBSVolumes[0].Size)
				}
			})
		},
	}
}
