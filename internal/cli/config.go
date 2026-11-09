package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/config"
	"github.com/migrx-io/backedblock/internal/estimate"
	"github.com/migrx-io/backedblock/internal/ui"
)

func (a *app) configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "create, list and delete storage configs",
	}
	cmd.AddCommand(a.withProfile(a.configCreateCmd()), a.configListCmd(), a.configDeleteCmd())
	return cmd
}

type configOpts struct {
	region, vpc, natSubnet, ami   string
	instanceType                  string
	azs                           []string
	volumes, volumeGB, mbps, iops int
	nodes                         int
	mgmt, force                   bool
}

// Names a storage cannot take: they are directories of a plain bbctl.yaml's
// work dir, which named storages share.
var reservedNames = []string{"stacks", "ssh"}

func (a *app) configCreateCmd() *cobra.Command {
	o := configOpts{}
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "write NAME.bbctl.yaml",
		Long: `Create the configuration for a storage cluster, NAME.bbctl.yaml. It creates
nothing in AWS: run ` + "`bbctl apply NAME`" + ` to create the resources.
Pools are sized by the calculator: https://backedblock.io/docs/sizing`,
		Example: `  bbctl config create prod
  bbctl config create prod --region us-east-1 --vpc vpc-0abc --volumes 40 --volume-size 8000 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runConfigCreate(cmd, args[0], o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.region, "region", "", "AWS region")
	f.StringVar(&o.vpc, "vpc", "", "VPC ID")
	f.StringSliceVar(&o.azs, "azs", nil, "AZs for the subnets; pools are spread across them")
	f.StringVar(&o.natSubnet, "nat-subnet", "", "public subnet for the NAT gateway")
	f.StringVar(&o.ami, "ami", "", "node AMI, see https://backedblock.io/docs/node-amis")
	f.IntVar(&o.volumes, "volumes", 0, "number of volumes")
	f.IntVar(&o.volumeGB, "volume-size", 0, "size of one volume, GB")
	f.IntVar(&o.mbps, "volume-throughput", 0, "read and write throughput per volume, MB/s")
	f.IntVar(&o.iops, "volume-iops", 0, "IOPS per volume")
	f.StringVar(&o.instanceType, "instance-type", "", "storage instance type")
	f.IntVar(&o.nodes, "nodes", 0, "pool's node count")
	f.BoolVar(&o.mgmt, "mgmt", false, "add a management plane")
	f.BoolVar(&o.force, "force", false, "overwrite an existing config")
	return cmd
}

type configResult struct {
	Config       string           `json:"config"`
	Storage      string           `json:"storage"`
	Account      string           `json:"account"`
	Estimate     *estimate.Result `json:"estimate,omitempty"`
	StorageClass string           `json:"storage_class,omitempty"`
	Settings     *config.Config   `json:"settings"`
	Next         []string         `json:"next"`
}

func (a *app) runConfigCreate(cmd *cobra.Command, name string, o configOpts) error {
	ctx := cmd.Context()
	flags := cmd.Flags()
	if !config.ValidName(name) || slices.Contains(reservedNames, name) {
		return ui.Errf(ui.ExitUsage, "use lowercase letters, digits and dashes, starting with a letter (max 31), not "+strings.Join(reservedNames, " or "),
			"invalid storage name %q", name)
	}
	path := config.StorageFile(".", name)
	if _, err := os.Stat(path); err == nil && !o.force {
		return ui.Errf(ui.ExitUsage, "pass --force to overwrite it", "%s already exists", path)
	}

	// Region and account.
	region := o.region
	if region == "" {
		if c, err := awsx.New(ctx, "", a.profile); err == nil {
			region = c.Region()
		}
		region = ask(flags, "region", "AWS region", region)
	}
	if region == "" {
		return ui.Errf(ui.ExitUsage, "pass --region", "no AWS region given or configured")
	}
	c, err := awsx.New(ctx, region, a.profile)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "configure AWS credentials (env vars, ~/.aws or `aws sso login`)", err)
	}
	id, err := c.Identity(ctx)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "configure AWS credentials (env vars, ~/.aws or `aws sso login`)", err)
	}
	ui.Logf("account %s, region %s", id.Account, c.Region())

	vpc, err := chooseVPC(ctx, c, o.vpc)
	if err != nil {
		return err
	}
	ui.Logf("vpc %s %v", vpc.ID, vpc.CIDRs)

	// Workload, priced and sized by the calculator.
	est, err := sizeWorkload(ctx, flags)
	if err != nil {
		return err
	}
	sized := config.DefaultPool(name, id.Account, "pool1", "")
	if err := yaml.Unmarshal([]byte(est.Config.Pool), &sized); err != nil {
		return ui.Wrap(ui.ExitError, "", fmt.Errorf("reading the calculator's pool config: %w", err))
	}
	var sc struct {
		Parameters map[string]string `yaml:"parameters"`
	}
	if err := yaml.Unmarshal([]byte(est.Config.StorageClass), &sc); err != nil {
		return ui.Wrap(ui.ExitError, "", fmt.Errorf("reading the calculator's StorageClass parameters: %w", err))
	}
	pools := 1
	if est.Split != nil {
		pools = est.Split.Pools
	}
	if o.instanceType != "" {
		sized.InstanceType = o.instanceType
	}
	if o.nodes > 0 {
		sized.NodesCount = o.nodes
	}

	var mgmt *config.Mgmt
	if flags.Changed("mgmt") {
		if o.mgmt {
			mgmt = config.DefaultMgmt()
		}
	} else if pools > 1 && askYes(fmt.Sprintf("The workload needs %d pools. Add a management plane in front of them?", pools), true) {
		mgmt = config.DefaultMgmt()
	}

	types := []string{sized.InstanceType}
	if mgmt != nil {
		types = append(types, mgmt.InstanceType)
	}
	azs := o.azs
	if len(azs) == 0 {
		offered, err := c.AZsOffering(ctx, types...)
		if err != nil {
			return ui.Wrap(ui.ExitAWS, "", err)
		}
		if len(offered) == 0 {
			return ui.Errf(ui.ExitAWS, "pick another type with --instance-type",
				"%v not offered in any AZ of %s", types, c.Region())
		}
		azs = strings.Split(ask(flags, "azs", "Availability zones", strings.Join(offered[:min(3, len(offered))], ",")), ",")
	}
	ui.Logf("azs %v", azs)

	subnets, err := c.Subnets(ctx, vpc.ID)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "", err)
	}
	// The NAT gateway goes in a public subnet, preferably in the first AZ.
	var used []string
	nat, natInFirstAZ := "", false
	for _, s := range subnets {
		used = append(used, s.CIDR)
		if s.Public && !natInFirstAZ && (nat == "" || s.AZ == azs[0]) {
			nat, natInFirstAZ = s.ID, s.AZ == azs[0]
		}
	}
	cidrs, err := awsx.FreeCIDRs(vpc.CIDRs, used, 2*len(azs))
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "add a secondary CIDR to the VPC or pick another --vpc", err)
	}
	if o.natSubnet != "" {
		nat = o.natSubnet
	} else {
		nat = ask(flags, "nat-subnet", "Public subnet for the NAT gateway", nat)
	}
	if nat == "" {
		return ui.Errf(ui.ExitAWS, "create a public subnet (route 0.0.0.0/0 to an internet gateway) and pass --nat-subnet",
			"VPC %s has no public subnet for the NAT gateway", vpc.ID)
	}
	ui.Logf("nat subnet %s", nat)

	ami := o.ami
	if ami == "" {
		arch, err := c.Arch(ctx, sized.InstanceType)
		if err != nil {
			return ui.Wrap(ui.ExitAWS, "", err)
		}
		im, err := awsx.PublishedAMI(ctx, c.Region(), arch)
		if err != nil {
			return ui.Wrap(ui.ExitAWS, "pass --ami; published IDs are at https://backedblock.io/docs/node-amis", err)
		}
		ami = im.ID
		ui.Logf("node ami %s (%s)", im.ID, im.Name)
	}

	cfg := &config.Config{
		Name:           name,
		Region:         c.Region(),
		Cluster:        "main",
		State:          config.State{Bucket: config.StateBucketName(name, id.Account, c.Region())},
		Modules:        config.Modules{Ref: config.DefaultModulesRef},
		NodesAMI:       ami,
		SecretsSSMPath: "/mgx/" + name + "/secrets",
		Network: config.Network{
			VPCID:              vpc.ID,
			AZs:                azs,
			MgmtSubnetCIDRs:    cidrs[:len(azs)],
			StorageSubnetCIDRs: cidrs[len(azs):],
			NATPublicSubnetID:  nat,
		},
		Mgmt: mgmt,
	}
	for i := range pools {
		pn := fmt.Sprintf("pool%d", i+1)
		p := config.DefaultPool(name, id.Account, pn, azs[i%len(azs)])
		p.NodesCount, p.InstanceType = sized.NodesCount, sized.InstanceType
		p.RaidLevel, p.MaxVolumesCount = sized.RaidLevel, sized.MaxVolumesCount
		p.RCacheSizeInMiB, p.RWCacheSizeInMiB = sized.RCacheSizeInMiB, sized.RWCacheSizeInMiB
		p.EBSVolumes = sized.EBSVolumes
		p.StorageClass = maps.Clone(sc.Parameters)
		cfg.Pools = append(cfg.Pools, p)
	}

	if errs := cfg.Validate(); len(errs) > 0 {
		e := ui.Errf(ui.ExitUsage, "override the offending values with flags", "generated config is invalid")
		e.Details = errs
		return e
	}
	if err := cfg.Save(path); err != nil {
		return err
	}

	res := configResult{
		Config: path, Storage: name, Account: id.Account, Estimate: est, Settings: cfg,
		StorageClass: est.Config.StorageClass,
		Next:         []string{"review " + path, "bbctl apply " + name},
	}
	return ui.Result(res, func(w io.Writer) { printConfigResult(w, res, vpc, pools) })
}

// sizeWorkload asks for the workload, defaulting to the calculator's own
// inputs, and prices it.
func sizeWorkload(ctx context.Context, flags *pflag.FlagSet) (*estimate.Result, error) {
	hint := "check network access to backedblock.io"
	defaults, err := estimate.Run(ctx, nil)
	if err != nil {
		return nil, ui.Wrap(ui.ExitError, hint, err)
	}
	def := func(key string) string { return fmt.Sprint(defaults.Inputs[key]) }

	in := estimate.Inputs{
		"volumeCount": ask(flags, "volumes", "Number of volumes", def("volumeCount")),
		"volumeGB":    ask(flags, "volume-size", "Size of one volume, GB", def("volumeGB")),
		"rwMBps":      ask(flags, "volume-throughput", "Throughput per volume, MB/s", def("writeMBps")),
		"iops":        ask(flags, "volume-iops", "IOPS per volume", def("iops")),
	}
	est, err := estimate.Run(ctx, in)
	var ie *estimate.InputError
	if errors.As(err, &ie) {
		e := ui.Errf(ui.ExitUsage, "fix the workload flags", "the calculator rejected the workload")
		e.Details = ie.Details
		return nil, e
	}
	if err != nil {
		return nil, ui.Wrap(ui.ExitError, hint, err)
	}
	if est.Config == nil || est.Sizing.Instance == nil {
		why := "nothing in the instance catalogue fits"
		if est.Sizing.Shortfall != nil {
			why = *est.Sizing.Shortfall
		}
		return nil, ui.Errf(ui.ExitUsage, "lower the per-volume throughput or IOPS", "the calculator cannot size this workload: %s", why)
	}
	return est, nil
}

func printConfigResult(w io.Writer, res configResult, vpc awsx.VPC, pools int) {
	cfg := res.Settings
	p := cfg.Pools[0]
	fmt.Fprintf(w, "wrote %s for account %s in %s\n\n", res.Config, res.Account, cfg.Region)
	fmt.Fprintf(w, "  vpc          %s\n  azs          %s\n  nat subnet   %s\n  node ami     %s\n",
		vpc.ID, strings.Join(cfg.Network.AZs, ", "), cfg.Network.NATPublicSubnetID, cfg.NodesAMI)
	if pools == 1 {
		fmt.Fprintf(w, "  pool         %s: %d x %s in %s\n", p.Name, p.NodesCount, p.InstanceType, p.AZ)
	} else {
		fmt.Fprintf(w, "  pools        %d, each %d x %s (pool1..pool%d)\n", pools, p.NodesCount, p.InstanceType, pools)
	}
	if cfg.Mgmt != nil {
		fmt.Fprintf(w, "  mgmt         %d x %s\n", cfg.Mgmt.NodesCount, cfg.Mgmt.InstanceType)
	}
	if est := res.Estimate; est != nil {
		in := est.Inputs
		fmt.Fprintf(w, "  workload     %v volumes x %v GB, %v MB/s, %v IOPS each\n", in["volumeCount"], in["volumeGB"], in["writeMBps"], in["iops"])
	}
	fmt.Fprintf(w, "\nnext:\n")
	for _, n := range res.Next {
		fmt.Fprintf(w, "  %s\n", n)
	}
}

func (a *app) configDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "delete NAME.bbctl.yaml",
		Long: `Delete removes NAME.bbctl.yaml. It does not touch AWS: if the storage is
deployed, run ` + "`bbctl destroy NAME`" + ` first, because destroy needs the config.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if !config.ValidName(name) {
				return ui.Errf(ui.ExitUsage, "see `bbctl config list`", "invalid storage name %q", name)
			}
			path := config.StorageFile(".", name)
			if _, err := os.Stat(path); err != nil {
				return ui.Errf(ui.ExitUsage, "see `bbctl config list`", "%s not found", path)
			}
			if !yes {
				if err := ui.Confirm("Delete "+path+"? AWS resources are not destroyed.", "--yes"); err != nil {
					return err
				}
			}
			if err := os.Remove(path); err != nil {
				return ui.Wrap(ui.ExitError, "", err)
			}
			res := map[string]string{"deleted": path}
			return ui.Result(res, func(w io.Writer) { fmt.Fprintf(w, "deleted %s\n", path) })
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete without asking")
	return cmd
}

func (a *app) configListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "list the storage configs in this directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			type entry struct {
				Storage string `json:"storage"`
				Config  string `json:"config"`
				Region  string `json:"region,omitempty"`
				Pools   int    `json:"pools"`
				Nodes   int    `json:"nodes"`
				Mgmt    bool   `json:"mgmt"`
				Error   string `json:"error,omitempty"`
			}
			names, err := config.Storages(".")
			if err != nil {
				return ui.Wrap(ui.ExitError, "", err)
			}
			files := map[string]string{}
			for _, n := range names {
				files[n] = config.StorageFile(".", n)
			}
			list := []entry{}
			for _, n := range names {
				e := entry{Storage: n, Config: files[n]}
				c, err := config.Load(files[n])
				if err != nil {
					e.Error = err.Error()
				} else {
					e.Region, e.Pools, e.Mgmt = c.Region, len(c.Pools), c.Mgmt != nil
					for _, p := range c.Pools {
						e.Nodes += p.NodesCount
					}
				}
				list = append(list, e)
			}
			return ui.Result(list, func(w io.Writer) {
				if len(list) == 0 {
					fmt.Fprintln(w, "no storage configs here; create one with `bbctl config create <name>`")
					return
				}
				for _, e := range list {
					if e.Error != "" {
						fmt.Fprintf(w, "%-16s %-24s error: %s\n", e.Storage, e.Config, e.Error)
						continue
					}
					fmt.Fprintf(w, "%-16s %-24s %-14s pools=%d nodes=%d mgmt=%t\n", e.Storage, e.Config, e.Region, e.Pools, e.Nodes, e.Mgmt)
				}
			})
		},
	}
}

func chooseVPC(ctx context.Context, c *awsx.Client, want string) (awsx.VPC, error) {
	vpcs, err := c.VPCs(ctx)
	if err != nil {
		return awsx.VPC{}, ui.Wrap(ui.ExitAWS, "", err)
	}
	var list []string
	def := ""
	for _, v := range vpcs {
		if v.ID == want {
			return v, nil
		}
		desc := v.ID + " " + strings.Join(v.CIDRs, ",")
		if v.Name != "" {
			desc += " (" + v.Name + ")"
		}
		if v.IsDefault {
			desc += " [default]"
			def = v.ID
		}
		list = append(list, desc)
	}
	switch {
	case want != "":
		return awsx.VPC{}, ui.Errf(ui.ExitAWS, "", "VPC %s not found in %s", want, c.Region())
	case len(vpcs) == 0:
		return awsx.VPC{}, ui.Errf(ui.ExitAWS, "create a VPC first", "no VPC in %s", c.Region())
	case len(vpcs) == 1:
		return vpcs[0], nil
	case ui.Interactive():
		fmt.Fprintf(os.Stderr, "VPCs in %s (pick the one your workloads run in):\n", c.Region())
		for i, d := range list {
			fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, d)
		}
		for {
			got := ui.Prompt("VPC (number or ID)", def)
			if n, err := strconv.Atoi(got); err == nil && n >= 1 && n <= len(vpcs) {
				return vpcs[n-1], nil
			}
			for _, v := range vpcs {
				if v.ID == got {
					return v, nil
				}
			}
			fmt.Fprintf(os.Stderr, "no VPC %q\n", got)
		}
	}
	e := ui.Errf(ui.ExitUsage, "pass --vpc with the VPC your workloads run in", "%d VPCs in %s; pick one", len(vpcs), c.Region())
	e.Details = list
	return awsx.VPC{}, e
}

// ask returns the flag's value when it was given, else asks (or, without a
// terminal, takes def).
func ask(flags *pflag.FlagSet, flag, question, def string) string {
	if flags.Changed(flag) {
		v := flags.Lookup(flag).Value.String()
		return strings.Trim(v, "[]") // string slices print as [a,b]
	}
	return ui.Prompt(question, def)
}

func askYes(question string, def bool) bool {
	d := "y/N"
	if def {
		d = "Y/n"
	}
	switch strings.ToLower(ui.Prompt(question+" ("+d+")", "")) {
	case "y", "yes":
		return true
	case "n", "no":
		return false
	}
	return def
}
