package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/deploy"
	"github.com/migrx-io/backedblock/internal/render"
	"github.com/migrx-io/backedblock/internal/ui"
)

func (a *app) statusCmd() *cobra.Command {
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "show each stack's outputs and its nodes' EC2, SSM and provisioning state",
		Long: `Status lists every stack, its outputs, and for each node its EC2 state, its
SSM agent status and the status of its provisioning run.`,
		Example: `  bbctl status prod
  bbctl apply prod --auto-approve && bbctl status prod --wait --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			bar := ui.Begin(d.Cfg.Name, "reading", len(d.Stacks))
			st, err := d.Status(ctx, bar.Step)
			bar.End("", err == nil)
			if err != nil {
				return err
			}
			if wait {
				if st, err = waitProvisioned(ctx, d, st, timeout); err != nil {
					return err
				}
			}
			return ui.Result(map[string]any{"deployment": d.Cfg.Name, "region": d.Cfg.Region, "provisioned": allProvisioned(st), "stacks": st}, func(w io.Writer) {
				for _, s := range st {
					state := "not applied"
					if s.Applied {
						state = "applied"
					}
					fmt.Fprintf(w, "%-16s %s\n", s.ID, state)
					for _, n := range s.Nodes {
						provision := orNone(n.Provision)
						if n.ProvisionDetail != "" {
							provision += " (" + n.ProvisionDetail + ")"
						}
						fmt.Fprintf(w, "  %-20s %-10s %-15s %-12s ssm=%-14s provision=%s\n", n.InstanceID, n.State, n.PrivateIP, n.AZ, orNone(n.SSM), provision)
					}
				}
			})
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait until every node is provisioned")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "how long --wait waits")
	return cmd
}

// waitProvisioned polls Status until every node is provisioned, one fails, or
// the timeout passes.
func waitProvisioned(ctx context.Context, d *deploy.Deployment, st []deploy.StackStatus, timeout time.Duration) (_ []deploy.StackStatus, err error) {
	deadline := time.Now().Add(timeout)
	bar := ui.Begin(d.Cfg.Name, "provisioning", countNodes(st))
	defer func() {
		if err != nil {
			bar.End("not provisioned", false)
		} else {
			bar.End("provisioned", true)
		}
	}()
	for {
		bar.Set(countProvisioned(st), countNodes(st))
		for _, s := range st {
			for _, n := range s.Nodes {
				if n.ProvisionFailed() {
					e := ui.Errf(ui.ExitError, fmt.Sprintf("inspect the node with `bbctl exec %s %s --node %s -- 'journalctl -n 200 --no-pager'`; fix and re-run `bbctl apply %s`", d.Cfg.Name, stackFlag(s.ID), n.InstanceID, d.Cfg.Name),
						"%s: provisioning failed on %s", s.ID, n.InstanceID)
					if n.ProvisionDetail != "" {
						e.Details = []string{n.ProvisionDetail}
					}
					return st, e
				}
			}
		}
		if allProvisioned(st) {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, ui.Errf(ui.ExitError, "re-run `bbctl status "+d.Cfg.Name+" --wait` to keep waiting", "nodes not provisioned after %s", timeout)
		}
		ui.Logf("waiting for provisioning: %d/%d nodes done", countProvisioned(st), countNodes(st))
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(15 * time.Second):
		}
		if st, err = d.Status(ctx, nil); err != nil {
			return st, err
		}
	}
}

// allProvisioned is true when at least one stack is applied and every node of
// every applied stack reports Success.
func allProvisioned(st []deploy.StackStatus) bool {
	n := countNodes(st)
	return n > 0 && countProvisioned(st) == n
}

func countNodes(st []deploy.StackStatus) int {
	n := 0
	for _, s := range st {
		n += len(s.Nodes)
	}
	return n
}

func countProvisioned(st []deploy.StackStatus) int {
	n := 0
	for _, s := range st {
		for _, node := range s.Nodes {
			if node.Provisioned() {
				n++
			}
		}
	}
	return n
}

// target selects nodes of one stack.
type target struct {
	pool string
	mgmt bool
	node string
	all  bool
}

func (t *target) flags(cmd *cobra.Command, allowAll bool) {
	f := cmd.Flags()
	f.StringVar(&t.pool, "pool", "", "target a pool's nodes (default: the first pool)")
	f.BoolVar(&t.mgmt, "mgmt", false, "target the management plane's nodes")
	f.StringVar(&t.node, "node", "", "node index (0-based) within the stack, or an instance ID from any stack (default: first SSM-online node)")
	if allowAll {
		f.BoolVar(&t.all, "all", false, "every node of the stack")
	}
}

// resolve returns the instance IDs to act on.
func (t *target) resolve(ctx context.Context, d *deploy.Deployment) ([]string, error) {
	// An instance ID names one node in the whole deployment, so without
	// --pool/--mgmt it is looked up in every stack, not just the first pool.
	if strings.HasPrefix(t.node, "i-") && !t.mgmt && t.pool == "" {
		return t.findInstance(ctx, d)
	}
	var stack *render.Stack
	for i, s := range d.Stacks {
		switch {
		case t.mgmt && s.Kind == render.KindMgmt,
			!t.mgmt && t.pool != "" && s.Pool == t.pool,
			!t.mgmt && t.pool == "" && s.Kind == render.KindPool && stack == nil:
			stack = &d.Stacks[i]
		}
	}
	if stack == nil {
		if t.mgmt {
			return nil, ui.Errf(ui.ExitUsage, "", "this deployment has no management plane")
		}
		return nil, ui.Errf(ui.ExitUsage, "", "no pool %q in the config", t.pool)
	}
	out, err := d.Outputs(ctx, *stack)
	if err != nil {
		return nil, err
	}
	ids := deploy.InstanceIDs(out)
	if len(ids) == 0 {
		return nil, ui.Errf(ui.ExitUsage, "run `bbctl apply "+d.Cfg.Name+"`", "%s has no nodes yet", stack.ID)
	}
	switch {
	case t.all:
		return ids, nil
	case strings.HasPrefix(t.node, "i-"):
		for _, id := range ids {
			if id == t.node {
				return []string{id}, nil
			}
		}
		return nil, ui.Errf(ui.ExitUsage, fmt.Sprintf("%s nodes: %v", stack.ID, ids), "%s is not a node of %s", t.node, stack.ID)
	case t.node != "":
		i, err := strconv.Atoi(t.node)
		if err != nil || i < 0 || i >= len(ids) {
			return nil, ui.Errf(ui.ExitUsage, fmt.Sprintf("%s has %d nodes", stack.ID, len(ids)), "bad --node %q", t.node)
		}
		return []string{ids[i]}, nil
	}
	nodes, err := d.AWS.Nodes(ctx, ids)
	if err != nil {
		return nil, ui.Wrap(ui.ExitAWS, "", err)
	}
	for _, n := range nodes {
		if n.SSM == "Online" {
			return []string{n.InstanceID}, nil
		}
	}
	return nil, ui.Errf(ui.ExitAWS, "check `bbctl status "+d.Cfg.Name+"`; nodes need outbound access to SSM through the NAT gateway", "no node of %s is online in SSM", stack.ID)
}

// findInstance looks for t.node in every applied pool and mgmt stack.
func (t *target) findInstance(ctx context.Context, d *deploy.Deployment) ([]string, error) {
	for _, s := range d.Stacks {
		if s.Kind == render.KindNetwork {
			continue
		}
		applied, err := d.AWS.StateExists(ctx, d.Cfg.State.Bucket, s.StateKey)
		if err != nil {
			return nil, ui.Wrap(ui.ExitAWS, "", err)
		}
		if !applied {
			continue
		}
		out, err := d.Outputs(ctx, s)
		if err != nil {
			return nil, err
		}
		for _, id := range deploy.InstanceIDs(out) {
			if id == t.node {
				return []string{id}, nil
			}
		}
	}
	return nil, ui.Errf(ui.ExitUsage, "`bbctl status "+d.Cfg.Name+"` lists every node with its instance ID", "%s is not a node of this deployment", t.node)
}

// stackFlag is the --pool/--mgmt flag that selects a stack by ID.
func stackFlag(id string) string {
	if id == "mgmt" {
		return "--mgmt"
	}
	return "--pool " + strings.TrimPrefix(id, "pool/")
}

func (a *app) execCmd() *cobra.Command {
	var t target
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "exec -- COMMAND...",
		Short: "run a shell command on nodes through SSM (non-interactive)",
		Long: `Exec runs a command as root on one node (default) or every node of a stack
(--all) through SSM Run Command and returns its exit code and output.`,
		Example: `  bbctl exec prod -- uptime
  bbctl exec prod --all --json -- 'df -h /'
  bbctl exec prod --mgmt --node 0 -- journalctl -n 50 --no-pager`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			ids, err := t.resolve(ctx, d)
			if err != nil {
				return err
			}
			res, err := d.AWS.Exec(ctx, ids, strings.Join(args, " "), timeout)
			if err != nil {
				return ui.Wrap(ui.ExitAWS, "", err)
			}
			failed := 0
			for _, r := range res {
				if r.Status != "Success" {
					failed++
				}
			}
			if err := ui.Result(map[string]any{"results": res}, func(w io.Writer) {
				for _, r := range res {
					if len(res) > 1 {
						fmt.Fprintf(w, "== %s (%s, exit %d)\n", r.InstanceID, r.Status, r.ExitCode)
					}
					fmt.Fprint(w, r.Stdout)
					if r.Stderr != "" {
						fmt.Fprint(w, r.Stderr)
					}
				}
			}); err != nil {
				return err
			}
			if failed > 0 {
				return &SilentExit{code: ui.ExitError}
			}
			return nil
		},
	}
	t.flags(cmd, true)
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "command timeout")
	return cmd
}

// forwardInstanceEnv names the instance for a forward started by `bbctl ui`,
// which has already resolved it.
const forwardInstanceEnv = "BBCTL_FORWARD_INSTANCE"

// forwardTo runs a port forward to a known instance. It needs only the
// config's region: no Terraform, so it does not contend for the stacks'
// working directories with another bbctl.
func (a *app) forwardTo(ctx context.Context, id string, remote, local int) error {
	cfg, err := a.load()
	if err != nil {
		return err
	}
	c, err := awsx.New(ctx, cfg.Region, a.profile)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "", err)
	}
	// Does not return on success: the session ends the process.
	if err := c.Session(ctx, id, "", remote, local, a.profile); err != nil {
		return ui.Wrap(ui.ExitAWS, "", err)
	}
	return nil
}

func (a *app) connectCmd() *cobra.Command {
	var t target
	var forward string
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "open a shell on a node, or forward a port from it, over SSM",
		Long: `Connect opens a login shell (bash) on a node over Session Manager, or with
--forward a local port forward.`,
		Example: `  bbctl connect prod
  bbctl connect prod --pool pool2 --node 1
  bbctl connect prod --forward 3001:3000`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			remote, local, err := parseForward(forward)
			if err != nil {
				return err
			}
			if id := os.Getenv(forwardInstanceEnv); id != "" && local > 0 {
				return a.forwardTo(ctx, id, remote, local)
			}
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			if local == 0 && !term.IsTerminal(int(os.Stdin.Fd())) {
				return ui.Errf(ui.ExitUsage, "use `bbctl exec "+d.Cfg.Name+" -- '<command>'` to run a command without a terminal",
					"an interactive shell needs a terminal")
			}
			ids, err := t.resolve(ctx, d)
			if err != nil {
				return err
			}
			if local > 0 {
				ui.Logf("forwarding localhost:%d -> %s:%d (Ctrl-C to stop)", local, ids[0], remote)
			} else {
				ui.Logf("connecting to %s", ids[0])
			}
			// Does not return on success: the session ends the process.
			if err := d.AWS.Session(ctx, ids[0], "", remote, local, a.profile); err != nil {
				return ui.Wrap(ui.ExitAWS, "check `bbctl status "+d.Cfg.Name+"`: the node must be running with ssm=Online", err)
			}
			return nil
		},
	}
	t.flags(cmd, false)
	cmd.Flags().StringVar(&forward, "forward", "", "forward [LOCAL:]REMOTE port instead of opening a shell")
	return cmd
}

func parseForward(s string) (remote, local int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	l, r, ok := strings.Cut(s, ":")
	if !ok {
		r = l
	}
	local, err1 := strconv.Atoi(l)
	remote, err2 := strconv.Atoi(r)
	if err1 != nil || err2 != nil || local <= 0 || remote <= 0 {
		return 0, 0, ui.Errf(ui.ExitUsage, "use --forward 3000 or --forward 3001:3000", "bad --forward %q", s)
	}
	return remote, local, nil
}

// SilentExit sets the exit code without printing an error: the result
// already reported the failure.
type SilentExit struct{ code int }

func (e *SilentExit) Error() string { return fmt.Sprintf("exit %d", e.code) }
func (e *SilentExit) ExitCode() int { return e.code }

func orNone(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
