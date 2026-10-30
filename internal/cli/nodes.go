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

	"github.com/migrx-io/backedblock/internal/deploy"
	"github.com/migrx-io/backedblock/internal/render"
	"github.com/migrx-io/backedblock/internal/ui"
)

func (a *app) statusCmd() *cobra.Command {
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show each stack's outputs and its nodes' EC2, SSM and provisioning state",
		Long: `status lists every stack, its outputs, and for each node its EC2 state, its
SSM agent status and the status of its provisioning run (setup-node.sh).

apply returns as soon as the provisioning run is created; the nodes configure
themselves afterwards. --wait polls until every node of every applied stack
reports provision=Success, and fails as soon as one reports Failed.`,
		Example: `  bbctl status
  bbctl apply --auto-approve && bbctl status --wait --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			st, err := d.Status(ctx)
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
						fmt.Fprintf(w, "  %-20s %-10s %-15s %-12s ssm=%-14s provision=%s\n", n.InstanceID, n.State, n.PrivateIP, n.AZ, orNone(n.SSM), orNone(n.Provision))
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
func waitProvisioned(ctx context.Context, d *deploy.Deployment, st []deploy.StackStatus, timeout time.Duration) ([]deploy.StackStatus, error) {
	deadline := time.Now().Add(timeout)
	for {
		for _, s := range st {
			for _, n := range s.Nodes {
				if n.ProvisionFailed() {
					e := ui.Errf(ui.ExitError, fmt.Sprintf("inspect the node with `bbctl exec %s --node %s -- 'journalctl -n 200 --no-pager'`; fix and re-run `bbctl apply`", stackFlag(s.ID), n.InstanceID),
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
			return st, ui.Errf(ui.ExitError, "re-run `bbctl status --wait` to keep waiting", "nodes not provisioned after %s", timeout)
		}
		ui.Logf("waiting for provisioning: %d/%d nodes done", countProvisioned(st), countNodes(st))
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(15 * time.Second):
		}
		var err error
		if st, err = d.Status(ctx); err != nil {
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
		return nil, ui.Errf(ui.ExitUsage, "run `bbctl apply`", "%s has no nodes yet", stack.ID)
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
	return nil, ui.Errf(ui.ExitAWS, "check `bbctl status`; nodes need outbound access to SSM through the NAT gateway", "no node of %s is online in SSM", stack.ID)
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
	return nil, ui.Errf(ui.ExitUsage, "`bbctl status` lists every node with its instance ID", "%s is not a node of this deployment", t.node)
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
		Use:   "exec [flags] -- COMMAND...",
		Short: "Run a shell command on nodes through SSM (non-interactive)",
		Long: `exec runs a command as root on one node (default) or every node of a stack
(--all) through SSM Run Command and returns its exit code and output. It needs
no SSH or bastion and no terminal, which makes it the way for scripts
and agents to operate the cluster. Exit code is 1 if the command failed on
any node.`,
		Example: `  bbctl exec -- uptime
  bbctl exec --all --json -- 'df -h /'
  bbctl exec --mgmt --node 0 -- journalctl -n 50 --no-pager`,
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

func (a *app) connectCmd() *cobra.Command {
	var t target
	var forward string
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Open a shell on a node, or forward a port from it, over SSM",
		Long: `connect starts an interactive Session Manager shell on a node, or with
--forward a local port forward (e.g. Grafana on 3000). Nodes have no public IP;
nothing inbound is opened. Run mgx-cli in the shell to manage the cluster.

The Session Manager protocol is built into bbctl: nothing else to install. A
shell needs a terminal; for scripts and agents use ` + "`bbctl exec`" + `, which runs
a command and returns its output. --forward runs until Ctrl-C.`,
		Example: `  bbctl connect
  bbctl connect --pool pool2 --node 1
  bbctl connect --forward 3000            # Grafana at http://localhost:3000
  bbctl connect --forward 3001:3000`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			remote, local, err := parseForward(forward)
			if err != nil {
				return err
			}
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			if local == 0 && !term.IsTerminal(int(os.Stdin.Fd())) {
				return ui.Errf(ui.ExitUsage, "use `bbctl exec -- '<command>'` to run a command without a terminal",
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
			if err := d.AWS.Session(ctx, ids[0], remote, local, a.profile); err != nil {
				return ui.Wrap(ui.ExitAWS, "check `bbctl status`: the node must be running with ssm=Online", err)
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
