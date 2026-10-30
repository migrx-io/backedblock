// Package cli defines the bbctl commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	profile    string
}

func NewRoot() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:   "bbctl",
		Short: "Deploy and operate backedblock.io storage clusters on AWS",
		Long: `bbctl deploys a backedblock.io storage cluster into your AWS account from a
single bbctl.yaml. It needs AWS credentials; it downloads Terraform and the
modules itself.

  bbctl init                 discover your VPC and write bbctl.yaml
  bbctl secrets generate     create the node secrets in SSM
  bbctl apply                create everything
  bbctl status               nodes and their health

Every command accepts --json: results go to stdout as JSON, progress goes to
stderr, and the exit code says what failed (2 config, 3 AWS, 4 terraform).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return ui.Wrap(ui.ExitUsage, "see `"+c.CommandPath()+" --help`", err)
	})
	pf := root.PersistentFlags()
	pf.StringVarP(&a.configPath, "config", "c", config.DefaultFile, "path to bbctl.yaml")
	pf.StringVar(&a.profile, "profile", "", "AWS shared config profile")
	pf.BoolVar(&ui.JSON, "json", false, "print results as JSON on stdout")
	pf.BoolVarP(&ui.Quiet, "quiet", "q", false, "no progress output on stderr")

	root.AddCommand(
		a.initCmd(), a.doctorCmd(), a.secretsCmd(),
		a.planCmd(), a.applyCmd(), a.destroyCmd(),
		a.statusCmd(), a.execCmd(), a.connectCmd(),
		a.poolCmd(), a.renderCmd(), versionCmd(),
	)
	return root
}

func (a *app) load() (*config.Config, error) {
	c, err := config.Load(a.configPath)
	if errors.Is(err, config.ErrNotFound) {
		return nil, ui.Errf(ui.ExitUsage, "run `bbctl init` to create one, or pass --config", "%s not found", a.configPath)
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
			v := map[string]string{"bbctl": Version, "terraform": tf.DefaultVersion, "modules_repo": tf.ModulesRepo}
			return ui.Result(v, func(w io.Writer) {
				fmt.Fprintf(w, "bbctl %s (terraform %s, modules %s)\n", Version, tf.DefaultVersion, tf.ModulesRepo)
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
