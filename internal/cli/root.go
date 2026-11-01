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
	storage    string
	profile    string
}

func NewRoot() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:   "bbctl",
		Short: "deploy and operate backedblock.io storage clusters on AWS",
		Long: `bbctl helps you to deploy and manage backedblock.io storage cluster

  bbctl init                 download Terraform and the modules
  bbctl config create NAME   generate NAME.bbctl.yaml
  bbctl apply NAME           create everything
  bbctl status NAME          nodes and their health`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return ui.Wrap(ui.ExitUsage, "see `"+c.CommandPath()+" --help`", err)
	})
	pf := root.PersistentFlags()
	pf.BoolVar(&ui.JSON, "json", false, "print results as JSON on stdout")
	pf.BoolVarP(&ui.Verbose, "verbose", "v", false, "show progress and Terraform output on stderr")

	root.AddCommand(
		a.initCmd(), a.configCmd(),
		a.withConfig(a.secretsCmd(), true),
		a.withConfig(a.planCmd(), true), a.withConfig(a.applyCmd(), true), a.withConfig(a.destroyCmd(), true),
		a.withConfig(a.statusCmd(), true), a.withConfig(a.execCmd(), true), a.withConfig(a.connectCmd(), true),
		a.withConfig(a.poolCmd(), true), versionCmd(),
	)
	root.InitDefaultHelpCmd() // lowercase, like every other command's Short
	for _, c := range root.Commands() {
		if c.Name() == "help" {
			c.Short = "help about any command"
		}
	}
	describe(root)
	return root
}

// withConfig makes a command (and its subcommands) take the config as its
// first argument, NAME for NAME.bbctl.yaml, plus --profile when the command
// calls AWS. The NAME is required: with several configs in one directory,
// guessing would plan or apply the wrong one.
func (a *app) withConfig(cmd *cobra.Command, aws bool) *cobra.Command {
	for _, sub := range cmd.Commands() {
		a.withConfig(sub, aws)
	}
	if cmd.HasSubCommands() {
		return cmd
	}
	if aws {
		a.withProfile(cmd)
	}

	verb, rest, _ := strings.Cut(cmd.Use, " ")
	cmd.Use = strings.TrimSpace(verb + " NAME " + rest)
	args, run := cmd.Args, cmd.RunE
	if args == nil {
		args = cobra.ExactArgs(0)
	}
	cmd.Args = func(c *cobra.Command, in []string) error {
		if len(in) == 0 || c.ArgsLenAtDash() == 0 {
			e := ui.Errf(ui.ExitUsage, "e.g. `"+c.CommandPath()+" <name>`; `bbctl config list` shows them", "missing the config NAME")
			e.Details, _ = config.Storages(".")
			return e
		}
		if err := args(c, in[1:]); err != nil {
			return ui.Wrap(ui.ExitUsage, "usage: "+c.UseLine(), err)
		}
		return nil
	}
	cmd.RunE = func(c *cobra.Command, in []string) error {
		a.storage = in[0]
		return run(c, in[1:])
	}
	return cmd
}

func (a *app) withProfile(cmd *cobra.Command) *cobra.Command {
	cmd.Flags().StringVar(&a.profile, "profile", "", "AWS profile")
	return cmd
}

// resolveConfig turns the NAME argument into its file, NAME.bbctl.yaml.
func (a *app) resolveConfig() error {
	if !config.ValidName(a.storage) {
		return ui.Errf(ui.ExitUsage, "see `bbctl config list`", "invalid config name %q", a.storage)
	}
	a.configPath = config.StorageFile(".", a.storage)
	return nil
}

func (a *app) load() (*config.Config, error) {
	if err := a.resolveConfig(); err != nil {
		return nil, err
	}
	c, err := config.Load(a.configPath)
	if errors.Is(err, config.ErrNotFound) {
		hint := "run `bbctl config create " + a.storage + "`, or see `bbctl config list`"
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
		Short: "print bbctl and pinned tool versions",
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
				fmt.Fprintf(w, "%-18s %s\n", "bbctl", Version)
				fmt.Fprintf(w, "%-18s %s\n", "terraform", tf.DefaultVersion)
				fmt.Fprintf(w, "%-18s %s\n", "terraform-aws-mgx", mod)
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

// describe gives every command without a Long its Short as a sentence:
// command lists show the Short in lowercase, a command's own help starts
// with a capital.
func describe(c *cobra.Command) {
	if c.Long == "" && c.Short != "" {
		c.Long = strings.ToUpper(c.Short[:1]) + c.Short[1:] + "."
	}
	for _, sub := range c.Commands() {
		describe(sub)
	}
}
