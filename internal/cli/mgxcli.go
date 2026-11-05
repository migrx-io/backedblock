package cli

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/migrx-io/backedblock/internal/deploy"
	"github.com/migrx-io/backedblock/internal/render"
	"github.com/migrx-io/backedblock/internal/ui"
)

func (a *app) cliCmd() *cobra.Command {
	var t target
	var user string
	cmd := &cobra.Command{
		Use:   "cli",
		Short: "open mgx-cli on a node, logged in to the cluster, over SSM",
		Long: `Cli opens mgx-cli, the cluster's command line, on a node over Session
Manager and starts it with "login admin --cluster <cluster> --ns <cluster>".
mgx-cli asks for the password; ` + "`bbctl secrets get NAME MGX_GW_ADMIN_PASSWD`" + `
prints it. The node is a management plane node, or without one the first
pool's; --mgmt, --pool and --node pick another. Type quit to leave.`,
		Example: `  bbctl cli prod
  bbctl cli prod --pool pool2 --node 1`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return ui.Errf(ui.ExitUsage, "use `bbctl exec "+d.Cfg.Name+" --mgmt -- '<command>'` to run a command without a terminal",
					"mgx-cli needs a terminal")
			}
			// The management plane's API reaches every pool, so it is the
			// default when there is one.
			if !t.mgmt && t.pool == "" && !strings.HasPrefix(t.node, "i-") {
				for _, s := range d.Stacks {
					if s.Kind == render.KindMgmt {
						t.mgmt = true
					}
				}
			}
			ids, err := t.resolve(ctx, d)
			if err != nil {
				return err
			}
			ui.Logf("opening mgx-cli on %s", ids[0])
			hint := "password: bbctl secrets get " + d.Cfg.Name + " " + scPasswordKey
			// Does not return on success: the session ends the process.
			if err := d.AWS.Session(ctx, ids[0], mgxCLICommand(user, d.Cfg.Cluster, hint), 0, 0, a.profile); err != nil {
				return ui.Wrap(ui.ExitAWS, "check `bbctl status "+d.Cfg.Name+"`: the node must be running with ssm=Online", err)
			}
			return nil
		},
	}
	t.flags(cmd, false)
	cmd.Flags().StringVar(&user, "user", "admin", "mgx-cli login name")
	return cmd
}

// mgxCLIStart runs mgx-cli as the node installed it, with its login queued
// as the first command. mgx-cli has no flag for that: it is a cmd.Cmd, so
// the login goes in Cmd.cmdqueue, which cmdloop runs before reading input.
// The user, cluster and hint come in as arguments, never as code; the login
// line quotes them for mgx-cli's shlex.
const mgxCLIStart = `import importlib.machinery as m, importlib.util as u, shlex, sys
user, cluster, hint = sys.argv[1:4]
sys.argv = ["mgx-cli"]
l = m.SourceFileLoader("mgx_cli", "/usr/bin/mgx-cli")
cli = u.module_from_spec(u.spec_from_loader(l.name, l))
l.exec_module(cli)
line = "login %s --cluster %s --ns %s" % (shlex.quote(user), shlex.quote(cluster), shlex.quote(cluster))
loop = cli.Cli.cmdloop
def start(self, intro=None):
    self.cmdqueue.append(line)
    self.intro = "%s\n%s\n%s%s" % (self.intro, hint, self.prompt, line)
    return loop(self, intro)
cli.Cli.cmdloop = start
cli.main()
`

// mgxCLICommand is the session's command: mgx-cli's own Python, read from
// its #! line (unquoted, so "/usr/bin/env python3" works too), running
// mgxCLIStart.
func mgxCLICommand(user, cluster, hint string) string {
	return `py=$(head -n1 /usr/bin/mgx-cli | cut -c3-) && exec $py -c ` +
		shellQuote(mgxCLIStart) + " " + shellQuote(user) + " " + shellQuote(cluster) + " " + shellQuote(hint)
}
