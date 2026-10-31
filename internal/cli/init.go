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
		Long: `init downloads the pinned Terraform and the terraform-aws-mgx modules into
bbctl's cache, so later commands have nothing to fetch. It does not call AWS
or write a config: create one with ` + "`bbctl config create <name>`" + `.

With a single config in the directory (or --storage / --config), init uses
its terraform and modules settings. Flags override them.`,
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
	Config           string   `json:"config,omitempty"`
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
	var tfPath string

	// A config's own settings win over the defaults, flags over both. No
	// config, or several, is fine: init then fetches the defaults.
	if a.resolveConfig() == nil {
		if c, err := config.Load(a.configPath); err == nil {
			res.Config = a.configPath
			tfPath = c.Terraform.Path
			if c.Terraform.Version != "" && !cmd.Flags().Changed("terraform-version") {
				o.terraformVersion = c.Terraform.Version
			}
			if !cmd.Flags().Changed("modules-ref") && !cmd.Flags().Changed("modules-path") {
				o.modulesRef, o.modulesPath = c.Modules.Ref, c.Modules.Path
			}
		}
	}

	var err error
	if res.Terraform, err = tf.EnsureTerraform(ctx, tfPath, o.terraformVersion); err != nil {
		return ui.Wrap(ui.ExitError, "check network access to releases.hashicorp.com, or set BBCTL_TERRAFORM to an existing binary", err)
	}
	if res.TerraformVersion, err = tf.TerraformVersion(ctx, res.Terraform); err != nil {
		return ui.Wrap(ui.ExitError, "", err)
	}

	if o.modulesPath == "" {
		res.ModulesRef = o.modulesRef
	}
	if res.ModulesDir, res.ModulesSHA, err = tf.EnsureModules(ctx, o.modulesRef, o.modulesPath); err != nil {
		return ui.Wrap(ui.ExitError, "check network access to github.com, or pass --modules-path with a local checkout", err)
	}

	res.Next = []string{"bbctl config create <name>"}
	if res.Config != "" {
		res.Next = []string{"bbctl doctor", "bbctl plan"}
	}
	return ui.Result(res, func(w io.Writer) {
		mod := shortSHA(res.ModulesSHA)
		switch {
		case o.modulesPath != "":
			mod = o.modulesPath
		case res.ModulesRef != "":
			mod = res.ModulesRef + " (" + mod + ")"
		}
		fmt.Fprintf(w, "%-18s %s\n", "COMPONENT", "VERSION")
		fmt.Fprintf(w, "%-18s %s\n", "terraform", res.TerraformVersion)
		fmt.Fprintf(w, "%-18s %s\n", "terraform-aws-mgx", mod)
	})
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
