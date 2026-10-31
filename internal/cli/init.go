package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/config"
	"github.com/migrx-io/backedblock/internal/tf"
	"github.com/migrx-io/backedblock/internal/ui"
)

type initOpts struct {
	modulesRef, modulesPath, terraformVersion string
}

func (a *app) initCmd() *cobra.Command {
	o := initOpts{}
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Download Terraform and the deployment modules",
		Example: `  bbctl init
  bbctl init --modules-ref v0.4.0`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runInit(cmd, o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.modulesRef, "modules-ref", config.DefaultModulesRef, "terraform-aws-mgx git ref")
	f.StringVar(&o.modulesPath, "modules-path", "", "use a local terraform-aws-mgx checkout")
	f.StringVar(&o.terraformVersion, "terraform-version", tf.DefaultVersion, "Terraform version")
	return cmd
}

type initResult struct {
	Terraform        string   `json:"terraform"`
	TerraformVersion string   `json:"terraform_version"`
	ModulesRef       string   `json:"modules_ref,omitempty"`
	ModulesSHA       string   `json:"modules_sha"`
	ModulesDir       string   `json:"modules_dir"`
	Next             []string `json:"next"`
}

func (a *app) runInit(cmd *cobra.Command, o initOpts) error {
	ctx := cmd.Context()
	res := initResult{}

	// Fetch both even if one fails, so the table shows each one's state.
	var tfErr, modErr error
	if res.Terraform, tfErr = tf.EnsureTerraform(ctx, "", o.terraformVersion); tfErr != nil {
		tfErr = ui.Wrap(ui.ExitError, "check network access to releases.hashicorp.com, or set BBCTL_TERRAFORM to an existing binary", tfErr)
	} else if res.TerraformVersion, tfErr = tf.TerraformVersion(ctx, res.Terraform); tfErr != nil {
		tfErr = ui.Wrap(ui.ExitError, "", tfErr)
	}
	if o.modulesPath == "" {
		res.ModulesRef = o.modulesRef
	}
	if res.ModulesDir, res.ModulesSHA, modErr = tf.EnsureModules(ctx, o.modulesRef, o.modulesPath); modErr != nil {
		hint := "check network access to github.com, or pass --modules-path with a local checkout"
		if o.modulesPath != "" {
			hint = "point --modules-path (or modules.path) at a terraform-aws-mgx checkout"
		}
		modErr = ui.Wrap(ui.ExitError, hint, modErr)
	}
	if ui.JSON {
		if tfErr != nil {
			return tfErr
		}
		if modErr != nil {
			return modErr
		}
	}

	res.Next = []string{"bbctl config create <name>"}
	mark := func(err error) string {
		if err != nil {
			return "✗"
		}
		return "✓"
	}
	if err := ui.Result(res, func(w io.Writer) {
		fmt.Fprintf(w, "%s terraform\n", mark(tfErr))
		fmt.Fprintf(w, "%s terraform-aws-mgx\n", mark(modErr))
	}); err != nil {
		return err
	}
	// The table is out; the errors and their hints follow on stderr.
	code := 0
	for _, err := range []error{tfErr, modErr} {
		if err != nil {
			if c := ui.PrintError(err); code == 0 {
				code = c
			}
		}
	}
	if code != 0 {
		return &SilentExit{code: code}
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
