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
		Short: "show what apply would change, without changing anything",
		Long:  `Plan renders the stacks and runs terraform plan on each.`,
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
		Short: "create or update the deployment",
		Long:  `Apply plans and applies each stack in order: network, pools, mgmt.`,
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
	var yes, purge bool
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "destroy the deployment (mgmt, pools, then network)",
		Long: `Destroy tears stacks down in reverse order. The node secret and the
Terraform state are kept; --purge deletes them too, once every stack is gone.`,
		Example: `  bbctl destroy prod
  bbctl destroy prod --purge`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if purge && len(stacks) > 0 {
				return ui.Errf(ui.ExitUsage, "drop --stack: --purge needs every stack destroyed", "--purge and --stack cannot be combined")
			}
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			only, err := stackSet(d, stacks)
			if err != nil {
				return err
			}
			// With --purge a missing state bucket is fine: a previous destroy
			// may have left only the secret behind.
			mode := deploy.BucketRequire
			if purge {
				mode = deploy.BucketIfExists
			}
			if err := d.Prepare(ctx, mode); err != nil {
				return err
			}
			var res []deploy.StackResult
			if d.HasState() {
				if res, err = d.Destroy(ctx, only, approver(yes, "destroy")); err != nil {
					_ = printResultsOnError(res)
					return err
				}
			}
			if !purge {
				return printResults(res)
			}
			p, err := purgeDeployment(ctx, d, yes)
			if err != nil {
				_ = printResultsOnError(res)
				return err
			}
			return printDestroy(res, p)
		},
	}
	cmd.Flags().StringSliceVar(&stacks, "stack", nil, "limit to these stacks (network, pool/<name>, mgmt)")
	cmd.Flags().BoolVar(&yes, "auto-approve", false, "destroy without asking")
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete the node secret and the Terraform state")
	return cmd
}

// purged is what --purge deleted.
type purged struct {
	Secret       string `json:"secret"`
	SecretStatus string `json:"secret_status"` // deleted or absent
	State        string `json:"state"`
	StateObjects int    `json:"state_objects"`
	Bucket       string `json:"bucket"`
	BucketStatus string `json:"bucket_status"` // deleted, kept (other state is in it) or absent
}

// purgeDeployment deletes the node secret and the deployment's state, after
// asking. It runs only after every stack is destroyed. The bucket goes too
// unless another deployment keeps its state there.
func purgeDeployment(ctx context.Context, d *deploy.Deployment, yes bool) (purged, error) {
	cfg := d.Cfg
	p := purged{Secret: cfg.SecretsSSMPath, State: "s3://" + cfg.State.Bucket + "/" + cfg.Name + "/", Bucket: "s3://" + cfg.State.Bucket}
	if !yes {
		if !ui.JSON {
			fmt.Fprintf(os.Stderr, "\npurge:\n")
			fmt.Fprintf(os.Stderr, "  %-8s node secret %s\n", "delete", p.Secret)
			fmt.Fprintf(os.Stderr, "  %-8s Terraform state %s (every version)\n", "delete", p.State)
			fmt.Fprintf(os.Stderr, "  %-8s bucket %s, if nothing else is in it\n", "delete", p.Bucket)
		}
		if err := ui.Confirm("purge "+cfg.Name+"?", "--auto-approve"); err != nil {
			return p, err
		}
	}
	existed, err := d.AWS.DeleteSecrets(ctx, cfg.SecretsSSMPath)
	if err != nil {
		return p, ui.Wrap(ui.ExitAWS, "", err)
	}
	p.SecretStatus = "deleted"
	if !existed {
		p.SecretStatus = "absent"
	}
	p.BucketStatus = "absent"
	if d.HasState() {
		deleted := false
		if p.StateObjects, deleted, err = d.AWS.PurgeState(ctx, cfg.State.Bucket, cfg.Name+"/"); err != nil {
			return p, ui.Wrap(ui.ExitAWS, "re-run `bbctl destroy "+cfg.Name+" --purge`", err)
		}
		p.BucketStatus = map[bool]string{true: "deleted", false: "kept"}[deleted]
	}
	return p, nil
}

func printDestroy(res []deploy.StackResult, p purged) error {
	out := struct {
		Stacks []deploy.StackResult `json:"stacks"`
		Purged purged               `json:"purged"`
	}{nonNilResults(res), p}
	return ui.Result(out, func(w io.Writer) {
		for _, r := range res {
			line := fmt.Sprintf("%-16s %s", r.ID, r.Status)
			if r.Reason != "" {
				line += " — " + r.Reason
			}
			fmt.Fprintln(w, line)
		}
		fmt.Fprintf(w, "%-16s %s\n", "node secret", map[string]string{"deleted": "deleted", "absent": "already gone"}[p.SecretStatus])
		fmt.Fprintf(w, "%-16s %d object versions deleted\n", "state", p.StateObjects)
		fmt.Fprintf(w, "%-16s %s\n", "state bucket", map[string]string{
			"deleted": "deleted", "kept": "kept: another deployment's state is in it", "absent": "already gone",
		}[p.BucketStatus])
	})
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
			// A plan is for review: show what it changes. Apply's resources were
			// already shown in its approval.
			if r.Status == "planned" && r.Plan != nil {
				for _, c := range r.Plan.Changes {
					fmt.Fprintf(w, "  %-8s %s\n", c.Action, c.Address)
				}
			}
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
