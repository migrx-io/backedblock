package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/config"
	"github.com/migrx-io/backedblock/internal/ui"
)

type initOpts struct {
	name, region, vpc, natSubnet, ami  string
	instanceType, poolName, modulesRef string
	modulesPath                        string
	azs                                []string
	mgmt, force                        bool
}

func (a *app) initCmd() *cobra.Command {
	o := initOpts{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Discover AWS values and write bbctl.yaml",
		Long: `init looks up everything bbctl.yaml needs in your AWS account: the VPC, AZs
where the instance type is offered, free subnet CIDRs, a public subnet for the
NAT gateway, and the node AMI listed at https://backedblock.io/docs/node-amis
for the region. It writes a config you can review and edit before
` + "`bbctl apply`" + `. Flags override any discovered value.`,
		Example: `  bbctl init --region us-east-1
  bbctl init --region eu-west-1 --vpc vpc-0abc --mgmt --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runInit(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.name, "name", "backedblock", "deployment name (prefixes resources and state)")
	f.StringVar(&o.region, "region", "", "AWS region (default: from your AWS config)")
	f.StringVar(&o.vpc, "vpc", "", "VPC ID (default: the only VPC in the region)")
	f.StringSliceVar(&o.azs, "azs", nil, "AZs for the subnets (default: up to 3 offering the instance type)")
	f.StringVar(&o.natSubnet, "nat-subnet", "", "public subnet for the NAT gateway (default: discovered)")
	f.StringVar(&o.ami, "ami", "", "node AMI (default: latest published for the instance type's architecture)")
	f.StringVar(&o.instanceType, "instance-type", config.DefaultInstanceType, "storage node instance type")
	f.StringVar(&o.poolName, "pool", "pool1", "name of the first pool")
	f.BoolVar(&o.mgmt, "mgmt", false, "add a management plane (needed for more than one pool)")
	f.StringVar(&o.modulesRef, "modules-ref", config.DefaultModulesRef, "terraform-aws-mgx git ref")
	f.StringVar(&o.modulesPath, "modules-path", "", "use a local terraform-aws-mgx checkout")
	f.BoolVar(&o.force, "force", false, "overwrite an existing bbctl.yaml")
	return cmd
}

type initResult struct {
	Config   string         `json:"config"`
	Account  string         `json:"account"`
	Settings *config.Config `json:"settings"`
	Next     []string       `json:"next"`
}

func (a *app) runInit(ctx context.Context, o initOpts) error {
	if _, err := os.Stat(a.configPath); err == nil && !o.force {
		return ui.Errf(ui.ExitUsage, "pass --force to overwrite it", "%s already exists", a.configPath)
	}
	c, err := awsx.New(ctx, o.region, a.profile)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "pass --region, or configure AWS credentials (env vars, ~/.aws or `aws sso login`)", err)
	}
	id, err := c.Identity(ctx)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "configure AWS credentials (env vars, ~/.aws or `aws sso login`)", err)
	}
	ui.Logf("account %s, region %s", id.Account, c.Region())

	vpc, err := pickVPC(ctx, c, o.vpc)
	if err != nil {
		return err
	}
	ui.Logf("vpc %s %v", vpc.ID, vpc.CIDRs)

	types := []string{o.instanceType}
	if o.mgmt {
		types = append(types, config.DefaultMgmtType)
	}
	azs := o.azs
	if len(azs) == 0 {
		offered, err := c.AZsOffering(ctx, types...)
		if err != nil {
			return ui.Wrap(ui.ExitAWS, "", err)
		}
		if len(offered) == 0 {
			return ui.Errf(ui.ExitAWS, "pick another --instance-type or --region", "%v not offered in any AZ of %s", types, c.Region())
		}
		azs = offered[:min(3, len(offered))]
	}
	ui.Logf("azs %v", azs)

	subnets, err := c.Subnets(ctx, vpc.ID)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "", err)
	}
	var used []string
	for _, s := range subnets {
		used = append(used, s.CIDR)
	}
	cidrs, err := awsx.FreeCIDRs(vpc.CIDRs, used, 2*len(azs))
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "add a secondary CIDR to the VPC or pick another --vpc", err)
	}

	nat := o.natSubnet
	if nat == "" {
		for _, s := range subnets {
			if s.Public && (nat == "" || s.AZ == azs[0]) {
				nat = s.ID
				if s.AZ == azs[0] {
					break
				}
			}
		}
		if nat == "" {
			return ui.Errf(ui.ExitAWS, "create a public subnet (route 0.0.0.0/0 to an internet gateway) and pass --nat-subnet",
				"VPC %s has no public subnet for the NAT gateway", vpc.ID)
		}
	}
	ui.Logf("nat subnet %s", nat)

	ami := o.ami
	if ami == "" {
		arch, err := c.Arch(ctx, o.instanceType)
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
		Name:           o.name,
		Region:         c.Region(),
		Cluster:        "main",
		State:          config.State{Bucket: config.StateBucketName(o.name, id.Account, c.Region())},
		Modules:        config.Modules{Ref: o.modulesRef, Path: o.modulesPath},
		NodesAMI:       ami,
		SecretsSSMPath: "/mgx/" + o.name + "/secrets",
		Network: config.Network{
			VPCID:              vpc.ID,
			AZs:                azs,
			MgmtSubnetCIDRs:    cidrs[:len(azs)],
			StorageSubnetCIDRs: cidrs[len(azs):],
			NATPublicSubnetID:  nat,
		},
	}
	pool := config.DefaultPool(o.name, id.Account, o.poolName, azs[0])
	pool.InstanceType = o.instanceType
	if o.mgmt {
		cfg.Mgmt = config.DefaultMgmt()
	} else {
		pool.EnableGrafana = true
	}
	cfg.Pools = []config.Pool{pool}

	if errs := cfg.Validate(); len(errs) > 0 {
		e := ui.Errf(ui.ExitUsage, "override the offending values with flags", "discovered config is invalid")
		e.Details = errs
		return e
	}
	if err := cfg.Save(a.configPath); err != nil {
		return err
	}
	res := initResult{
		Config: a.configPath, Account: id.Account, Settings: cfg,
		Next: []string{"review " + a.configPath, "bbctl secrets generate", "bbctl doctor", "bbctl apply"},
	}
	return ui.Result(res, func(w io.Writer) {
		fmt.Fprintf(w, "wrote %s for account %s in %s\n\n", a.configPath, id.Account, c.Region())
		fmt.Fprintf(w, "  vpc          %s\n  azs          %s\n  nat subnet   %s\n  node ami     %s\n  pool         %s: %d x %s in %s\n",
			vpc.ID, strings.Join(azs, ", "), nat, ami, pool.Name, pool.NodesCount, pool.InstanceType, pool.AZ)
		if cfg.Mgmt != nil {
			fmt.Fprintf(w, "  mgmt         %d x %s\n", cfg.Mgmt.NodesCount, cfg.Mgmt.InstanceType)
		}
		fmt.Fprintf(w, "\nnext:\n")
		for _, n := range res.Next {
			fmt.Fprintf(w, "  %s\n", n)
		}
	})
}

func pickVPC(ctx context.Context, c *awsx.Client, want string) (awsx.VPC, error) {
	vpcs, err := c.VPCs(ctx)
	if err != nil {
		return awsx.VPC{}, ui.Wrap(ui.ExitAWS, "", err)
	}
	var list []string
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
		}
		list = append(list, desc)
	}
	if want != "" {
		return awsx.VPC{}, ui.Errf(ui.ExitAWS, "", "VPC %s not found in %s", want, c.Region())
	}
	if len(vpcs) == 1 {
		return vpcs[0], nil
	}
	if len(vpcs) == 0 {
		return awsx.VPC{}, ui.Errf(ui.ExitAWS, "create a VPC first", "no VPC in %s", c.Region())
	}
	e := ui.Errf(ui.ExitUsage, "pass --vpc with the VPC your workloads run in", "%d VPCs in %s; pick one", len(vpcs), c.Region())
	e.Details = list
	return awsx.VPC{}, e
}
