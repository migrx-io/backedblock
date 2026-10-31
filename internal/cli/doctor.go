package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/config"
	"github.com/migrx-io/backedblock/internal/tf"
	"github.com/migrx-io/backedblock/internal/ui"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check that everything apply needs is in place",
		Long: `doctor checks credentials, the config, the toolchain and the AWS resources
bbctl.yaml refers to, and says how to fix each problem. It changes nothing
(except downloading terraform into the cache). Exit code 1 if any check fails.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := a.runDoctor(cmd.Context())
			failed := 0
			for _, c := range checks {
				if c.Status == "fail" {
					failed++
				}
			}
			err := ui.Result(map[string]any{"ok": failed == 0, "checks": checks}, func(w io.Writer) {
				for _, c := range checks {
					mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[c.Status]
					fmt.Fprintf(w, "%s %-22s %s\n", mark, c.Name, c.Detail)
					if c.Hint != "" && c.Status != "ok" {
						fmt.Fprintf(w, "  → %s\n", c.Hint)
					}
				}
			})
			if err != nil {
				return err
			}
			if failed > 0 {
				return &SilentExit{code: ui.ExitError}
			}
			return nil
		},
	}
}

func (a *app) runDoctor(ctx context.Context) []check {
	var cs []check
	add := func(name, status, detail, hint string) { cs = append(cs, check{name, status, detail, hint}) }

	cfg, err := a.load()
	if err != nil {
		detail, hint := err.Error(), "run `bbctl config create <name>`"
		var ue *ui.Error
		if errors.As(err, &ue) {
			if len(ue.Details) > 0 {
				detail += ": " + strings.Join(ue.Details, ", ")
			}
			if ue.Hint != "" {
				hint = ue.Hint
			}
		}
		add("config", "fail", detail, hint)
		return cs
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		add("config", "fail", strings.Join(errs, "; "), "fix "+a.configPath)
		return cs
	}
	add("config", "ok", a.configPath, "")

	c, err := awsx.New(ctx, cfg.Region, a.profile)
	if err != nil {
		add("aws credentials", "fail", err.Error(), "configure AWS credentials (env vars, ~/.aws or `aws sso login`)")
		return cs
	}
	id, err := c.Identity(ctx)
	if err != nil {
		add("aws credentials", "fail", err.Error(), "configure AWS credentials (env vars, ~/.aws or `aws sso login`)")
		return cs
	}
	add("aws credentials", "ok", id.ARN, "")

	if bin, err := tf.EnsureTerraform(ctx, cfg.Terraform.Path, cfg.Terraform.Version); err != nil {
		add("terraform", "fail", err.Error(), "set terraform.path in bbctl.yaml to an existing binary")
	} else if v, err := tf.TerraformVersion(ctx, bin); err != nil {
		add("terraform", "fail", err.Error(), "")
	} else {
		add("terraform", "ok", v+" ("+bin+")", "")
	}

	if exists, err := c.StateBucketExists(ctx, cfg.State.Bucket); err != nil {
		add("state bucket", "fail", err.Error(), "")
	} else if exists {
		add("state bucket", "ok", "s3://"+cfg.State.Bucket, "")
	} else {
		add("state bucket", "warn", "s3://"+cfg.State.Bucket+" does not exist yet", "`bbctl apply` creates it")
	}

	if s, found, err := c.GetSecrets(ctx, cfg.SecretsSSMPath); err != nil {
		add("node secret", "fail", err.Error(), "needs ssm:GetParameter and kms:Decrypt")
	} else if !found {
		add("node secret", "fail", cfg.SecretsSSMPath+" does not exist", "run `bbctl secrets generate`")
	} else if miss := awsx.MissingSecrets(s); len(miss) > 0 {
		add("node secret", "fail", "missing "+strings.Join(miss, ", "), "run `bbctl secrets set KEY=VALUE`")
	} else {
		add("node secret", "ok", cfg.SecretsSSMPath, "")
	}

	add(checkVPC(ctx, c, cfg))
	add(checkAMI(ctx, c, cfg))
	for _, p := range cfg.Pools {
		add(checkOffered(ctx, c, "pool "+p.Name, p.InstanceType, p.AZ))
	}
	if cfg.Mgmt != nil {
		offered, err := c.AZsOffering(ctx, cfg.Mgmt.InstanceType)
		switch {
		case err != nil:
			add("mgmt instance type", "fail", err.Error(), "")
		case !overlap(offered, cfg.Network.AZs):
			add("mgmt instance type", "fail", cfg.Mgmt.InstanceType+" not offered in "+strings.Join(cfg.Network.AZs, ", "), "change mgmt.instance_type")
		default:
			add("mgmt instance type", "ok", cfg.Mgmt.InstanceType, "")
		}
	}

	return cs
}

func checkVPC(ctx context.Context, c *awsx.Client, cfg *config.Config) (string, string, string, string) {
	subnets, err := c.Subnets(ctx, cfg.Network.VPCID)
	if err != nil {
		return "vpc", "fail", err.Error(), ""
	}
	if len(subnets) == 0 {
		return "vpc", "fail", cfg.Network.VPCID + " not found or has no subnets", "check network.vpc_id and region"
	}
	ours := append(append([]string{}, cfg.Network.MgmtSubnetCIDRs...), cfg.Network.StorageSubnetCIDRs...)
	var nat *awsx.Subnet
	var clashes []string
	for i, s := range subnets {
		if s.ID == cfg.Network.NATPublicSubnetID {
			nat = &subnets[i]
		}
		// Subnets this deployment already created have exactly our CIDRs; any
		// other subnet with an overlapping CIDR blocks the network stack.
		if !strings.HasPrefix(s.Name, cfg.Name+"-") {
			for _, o := range ours {
				if overlapsCIDR(o, s.CIDR) {
					clashes = append(clashes, fmt.Sprintf("%s overlaps %s (%s)", o, s.ID, s.CIDR))
				}
			}
		}
	}
	if len(clashes) > 0 {
		return "vpc", "fail", strings.Join(clashes, "; "), "pick free CIDRs, or re-run `bbctl config create <name> --force` to discover them"
	}
	if nat == nil {
		return "vpc", "fail", cfg.Network.NATPublicSubnetID + " is not a subnet of " + cfg.Network.VPCID, "set network.nat_public_subnet_id to a public subnet in that VPC"
	}
	if !nat.Public {
		return "vpc", "fail", nat.ID + " has no active 0.0.0.0/0 route to an attached internet gateway", "fix its route table, or set network.nat_public_subnet_id to a public subnet"
	}
	return "vpc", "ok", fmt.Sprintf("%s, NAT in %s", cfg.Network.VPCID, cfg.Network.NATPublicSubnetID), ""
}

func checkAMI(ctx context.Context, c *awsx.Client, cfg *config.Config) (string, string, string, string) {
	im, err := c.Image(ctx, cfg.NodesAMI)
	if err != nil {
		return "node ami", "fail", err.Error(), "see https://backedblock.io/docs/node-amis for your region"
	}
	types := []string{}
	for _, p := range cfg.Pools {
		types = append(types, p.InstanceType)
	}
	if cfg.Mgmt != nil {
		types = append(types, cfg.Mgmt.InstanceType)
	}
	for _, t := range types {
		arch, err := c.Arch(ctx, t)
		if err != nil {
			return "node ami", "fail", err.Error(), ""
		}
		if arch != im.Arch {
			return "node ami", "fail", fmt.Sprintf("%s is %s but %s is %s", cfg.NodesAMI, im.Arch, t, arch), "use an AMI matching the instance architecture"
		}
	}
	latest, err := awsx.PublishedAMI(ctx, c.Region(), im.Arch)
	if err == nil && latest.ID != im.ID && latest.Created > im.Created {
		return "node ami", "warn", fmt.Sprintf("%s (%s); newer: %s", im.ID, im.Name, latest.ID), "set nodes_ami: " + latest.ID + " to upgrade"
	}
	return "node ami", "ok", fmt.Sprintf("%s (%s)", im.ID, im.Name), ""
}

func checkOffered(ctx context.Context, c *awsx.Client, name, instanceType, az string) (string, string, string, string) {
	offered, err := c.AZsOffering(ctx, instanceType)
	if err != nil {
		return name, "fail", err.Error(), ""
	}
	for _, z := range offered {
		if z == az {
			return name, "ok", instanceType + " in " + az, ""
		}
	}
	return name, "fail", instanceType + " is not offered in " + az, "offered in: " + strings.Join(offered, ", ")
}

func overlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

func overlapsCIDR(a, b string) bool {
	pa, err1 := netip.ParsePrefix(a)
	pb, err2 := netip.ParsePrefix(b)
	return err1 == nil && err2 == nil && pa.Overlaps(pb)
}
