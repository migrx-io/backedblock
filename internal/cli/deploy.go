package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/deploy"
	"github.com/migrx-io/backedblock/internal/tf"
	"github.com/migrx-io/backedblock/internal/ui"
)

func (a *app) planCmd() *cobra.Command {
	var stacks []string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what apply would change, without changing anything",
		Long: `plan renders the stacks and runs terraform plan on each. Pools and mgmt read
the network's outputs, so before the network exists they are reported as
skipped and planned during apply instead.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			only, err := stackSet(d, stacks)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketIfExists); err != nil {
				return err
			}
			res, err := d.Plan(ctx, only)
			if err != nil {
				return err
			}
			return printResults(res)
		},
	}
	cmd.Flags().StringSliceVar(&stacks, "stack", nil, "limit to these stacks (network, pool/<name>, mgmt)")
	return cmd
}

func (a *app) applyCmd() *cobra.Command {
	var stacks []string
	var yes bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Create or update the deployment",
		Long: `apply plans and applies each stack in order: network, pools, mgmt.

It asks before each change; --auto-approve skips the prompts (required
without a TTY). Nothing is created before an approval: a missing node secret
is created once you approve it, and on a first deploy the Terraform state
bucket is listed in the network plan and created once that plan is approved.
Re-running is safe: stacks with no changes are skipped.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			only, err := stackSet(d, stacks)
			if err != nil {
				return err
			}
			if err := ensureSecrets(ctx, d, yes); err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketIfExists); err != nil {
				return err
			}
			res, err := d.Apply(ctx, only, approver(yes, "apply"))
			if err != nil {
				_ = printResultsOnError(res)
				return err
			}
			return printResults(res)
		},
	}
	cmd.Flags().StringSliceVar(&stacks, "stack", nil, "limit to these stacks (network, pool/<name>, mgmt)")
	cmd.Flags().BoolVar(&yes, "auto-approve", false, "apply without asking")
	return cmd
}

func (a *app) destroyCmd() *cobra.Command {
	var stacks []string
	var yes bool
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Destroy the deployment (mgmt, pools, then network)",
		Long: `destroy tears stacks down in reverse order. Pools are created with
s3_force_destroy, so their buckets AND THE VOLUME DATA IN THEM are deleted.
The state bucket and the node secret are kept.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			only, err := stackSet(d, stacks)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			res, err := d.Destroy(ctx, only, approver(yes, "destroy"))
			if err != nil {
				_ = printResultsOnError(res)
				return err
			}
			return printResults(res)
		},
	}
	cmd.Flags().StringSliceVar(&stacks, "stack", nil, "limit to these stacks (network, pool/<name>, mgmt)")
	cmd.Flags().BoolVar(&yes, "auto-approve", false, "destroy without asking")
	return cmd
}

func approver(yes bool, verb string) deploy.Approve {
	return func(id string, p tf.PlanSummary) error {
		if yes {
			return nil
		}
		if !ui.JSON {
			fmt.Fprintf(os.Stderr, "\n%s: +%d to add, ~%d to change, -%d to destroy\n", id, p.Add, p.Change, p.Destroy)
			for _, c := range p.Changes {
				fmt.Fprintf(os.Stderr, "  %-8s %s\n", c.Action, c.Address)
			}
		}
		return ui.Confirm(fmt.Sprintf("%s %s?", verb, id), "--auto-approve")
	}
}

type results struct {
	Stacks []deploy.StackResult `json:"stacks"`
}

func printResults(res []deploy.StackResult) error {
	return ui.Result(results{Stacks: nonNilResults(res)}, func(w io.Writer) {
		for _, r := range res {
			line := fmt.Sprintf("%-16s %s", r.ID, r.Status)
			if r.Plan != nil && !r.Plan.Empty() {
				line += fmt.Sprintf(" (+%d ~%d -%d)", r.Plan.Add, r.Plan.Change, r.Plan.Destroy)
			}
			if r.Reason != "" {
				line += " — " + r.Reason
			}
			fmt.Fprintln(w, line)
		}
	})
}

// printResultsOnError reports finished stacks before the error, in text mode
// only: in JSON mode stdout carries exactly one document, the error.
func printResultsOnError(res []deploy.StackResult) error {
	if ui.JSON || len(res) == 0 {
		return nil
	}
	return printResults(res)
}

func nonNilResults(r []deploy.StackResult) []deploy.StackResult {
	if r == nil {
		return []deploy.StackResult{}
	}
	return r
}

func (a *app) renderCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "render",
		Short:  "Write the Terraform stacks to .bbctl/<name>/stacks without running them",
		Hidden: true, // runs, but kept out of the command list
		Long: `render resolves the modules and writes each stack's main.tf.json, for review
or to run Terraform by hand. It needs no AWS access beyond config validation.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Render(ctx); err != nil {
				return err
			}
			type stack struct {
				ID  string `json:"id"`
				Dir string `json:"dir"`
			}
			var out []stack
			for _, s := range d.Stacks {
				out = append(out, stack{s.ID, s.Dir})
			}
			return ui.Result(map[string]any{"modules": d.ModulesSHA(), "stacks": out}, func(w io.Writer) {
				for _, s := range out {
					fmt.Fprintf(w, "%-16s %s\n", s.ID, s.Dir)
				}
			})
		},
	}
}

// ensureSecrets creates the node secret on a first apply, after asking like
// any other resource. An existing secret is only checked, never replaced.
func ensureSecrets(ctx context.Context, d *deploy.Deployment, yes bool) error {
	path := d.Cfg.SecretsSSMPath
	_, found, err := d.AWS.GetSecrets(ctx, path)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "", fmt.Errorf("read %s: %w", path, err))
	}
	if !found {
		if !yes {
			if err := ui.Confirm("Create the node secret "+path+" in SSM?", "--auto-approve"); err != nil {
				return err
			}
		}
		s, err := awsx.GenerateSecrets()
		if err != nil {
			return err
		}
		if err := d.AWS.PutSecrets(ctx, path, s); err != nil {
			return ui.Wrap(ui.ExitAWS, "", err)
		}
		ui.Logf("created node secret %s", path)
	}
	return d.CheckSecrets(ctx)
}
