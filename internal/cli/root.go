// Package cli defines the bbctl commands.
package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/config"
	"github.com/migrx-io/backedblock/internal/deploy"
	"github.com/migrx-io/backedblock/internal/tf"
	"github.com/migrx-io/backedblock/internal/ui"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "dev"

type app struct {
	configPath string
	storage    string
	profile    string
}

func NewRoot() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:   "bbctl",
		Short: "Deploy and operate backedblock.io storage clusters on AWS",
		Long: `bbctl deploys a backedblock.io storage cluster into your AWS account from a
storage config, NAME.bbctl.yaml.

  bbctl init                 download Terraform and the modules
  bbctl config create NAME   generate NAME.bbctl.yaml
  bbctl secrets generate     create the node secrets in SSM
  bbctl apply                create everything
  bbctl status               nodes and their health`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return ui.Wrap(ui.ExitUsage, "see `"+c.CommandPath()+" --help`", err)
	})
	pf := root.PersistentFlags()
	pf.StringVarP(&a.storage, "storage", "s", "", "named storage: uses <name>.bbctl.yaml (default: the only one here)")
	pf.StringVarP(&a.configPath, "config", "c", "", "path to a config file, instead of --storage")
	pf.StringVar(&a.profile, "profile", "", "AWS shared config profile")
	pf.BoolVar(&ui.JSON, "json", false, "print results as JSON on stdout")
	pf.BoolVarP(&ui.Quiet, "quiet", "q", false, "no progress output on stderr")

	root.AddCommand(
		a.initCmd(), a.configCmd(), a.doctorCmd(), a.secretsCmd(),
		a.planCmd(), a.applyCmd(), a.destroyCmd(),
		a.statusCmd(), a.execCmd(), a.connectCmd(),
		a.poolCmd(), a.renderCmd(), versionCmd(),
	)
	return root
}

// resolveConfig settles which config file a command works on: --config as
// given, --storage's <name>.bbctl.yaml, else the only config in the directory
// (a named storage or a plain bbctl.yaml). Several is an error, not a guess.
func (a *app) resolveConfig() error {
	switch {
	case a.configPath != "" && a.storage != "":
		return ui.Errf(ui.ExitUsage, "pass one of them", "--config and --storage both given")
	case a.configPath != "":
		return nil
	case a.storage != "":
		if !config.ValidName(a.storage) {
			return ui.Errf(ui.ExitUsage, "see `bbctl config list`", "invalid storage name %q", a.storage)
		}
		a.configPath = config.StorageFile(".", a.storage)
		return nil
	}
	names, err := config.Storages(".")
	if err != nil {
		return ui.Wrap(ui.ExitError, "", err)
	}
	files := make([]string, len(names))
	for i, n := range names {
		files[i] = config.StorageFile(".", n)
	}
	if _, err := os.Stat(config.DefaultFile); err == nil {
		files = append(files, config.DefaultFile)
	}
	switch len(files) {
	case 0:
		a.configPath = config.DefaultFile // not found; load says how to create one
		return nil
	case 1:
		a.configPath = files[0]
		return nil
	}
	e := ui.Errf(ui.ExitUsage, "pass --storage <name> (or --config <file>)", "%d configs here; pick one", len(files))
	e.Details = files
	return e
}

func (a *app) load() (*config.Config, error) {
	if err := a.resolveConfig(); err != nil {
		return nil, err
	}
	c, err := config.Load(a.configPath)
	if errors.Is(err, config.ErrNotFound) {
		hint := "run `bbctl config create <name>` to create one"
		if a.storage != "" {
			hint = "run `bbctl config create " + a.storage + "`, or see `bbctl config list`"
		}
		return nil, ui.Errf(ui.ExitUsage, hint, "%s not found", a.configPath)
	}
	if err != nil {
		return nil, ui.Wrap(ui.ExitUsage, "", err)
	}
	return c, nil
}

func (a *app) open(ctx context.Context) (*deploy.Deployment, error) {
	c, err := a.load()
	if err != nil {
		return nil, err
	}
	return deploy.Open(ctx, c, a.configPath, a.profile)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print bbctl and pinned tool versions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ref := config.DefaultModulesRef
			sha := tf.CachedModulesSHA(ref)
			v := map[string]string{"bbctl": Version, "terraform": tf.DefaultVersion, "modules_repo": tf.ModulesRepo, "modules_ref": ref}
			mod := ref
			if sha != "" {
				v["modules_sha"] = sha
				mod += " (" + shortSHA(sha) + ")"
			}
			return ui.Result(v, func(w io.Writer) {
				ui.Table(w, []string{"COMPONENT", "VERSION"},
					[]string{"bbctl", Version},
					[]string{"terraform", tf.DefaultVersion},
					[]string{"terraform-aws-mgx", mod})
			})
		},
	}
}

// stackSet turns --stack flags into a lookup, rejecting unknown IDs.
func stackSet(d *deploy.Deployment, ids []string) (map[string]bool, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	known := map[string]bool{}
	var names []string
	for _, s := range d.Stacks {
		known[s.ID] = true
		names = append(names, s.ID)
	}
	set := map[string]bool{}
	for _, id := range ids {
		if !known[id] {
			return nil, ui.Errf(ui.ExitUsage, "stacks: "+strings.Join(names, ", "), "unknown stack %q", id)
		}
		set[id] = true
	}
	return set, nil
}
