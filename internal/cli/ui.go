package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/dash"
	"github.com/migrx-io/backedblock/internal/deploy"
	"github.com/migrx-io/backedblock/internal/render"
	"github.com/migrx-io/backedblock/internal/ui"
)

// Ports on the nodes: the cluster API (mgx-gateway-api) and Prometheus.
const (
	gatewayPort    = 8082
	prometheusPort = 9090
)

func (a *app) uiCmd() *cobra.Command {
	var pool string
	var port int
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "ui",
		Short: "open a dashboard of a pool's cluster in the browser",
		Long: `Open a dashboard of a pool's cluster in the browser: its nodes, their health
and metrics. bbctl reaches the cluster API and Prometheus over SSM port
forwards and serves the page on 127.0.0.1 until Ctrl-C.`,
		Example: `  bbctl ui prod
  bbctl ui prod --pool pool2 --port 8081`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			d, err := a.open(ctx)
			if err != nil {
				return err
			}
			if err := d.Prepare(ctx, deploy.BucketRequire); err != nil {
				return err
			}
			cfg, err := a.uiConfig(ctx, d, pool)
			if err != nil {
				return err
			}
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				return ui.Wrap(ui.ExitUsage, "pick another port with --port", err)
			}
			url := "http://" + ln.Addr().String()

			srv := &http.Server{Handler: dash.New(cfg).Handler(), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-ctx.Done()
				_ = srv.Close()
			}()
			if err := ui.Result(map[string]string{"url": url, "pool": cfg.Pool}, func(w io.Writer) {
				fmt.Fprintf(w, "dashboard for %s/%s at %s (Ctrl-C to stop)\n", d.Cfg.Name, cfg.Pool, url)
			}); err != nil {
				return err
			}
			if !noOpen && !ui.JSON {
				openBrowser(url)
			}
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&pool, "pool", "", "pool to show (default: the first pool)")
	f.IntVar(&port, "port", 8080, "local port for the dashboard")
	f.BoolVar(&noOpen, "no-open", false, "do not open a browser")
	return cmd
}

// uiConfig picks the nodes to read from and starts the port forwards to them.
// The cluster API is any SSM-online node of the pool. Prometheus is the
// management plane's when there is one: pools under mgmt keep only their own
// node's series, and mgmt federates them all, labelled by pool.
func (a *app) uiConfig(ctx context.Context, d *deploy.Deployment, poolName string) (dash.Config, error) {
	var poolStack, mgmtStack *render.Stack
	var pools []string
	for i, s := range d.Stacks {
		switch s.Kind {
		case render.KindPool:
			pools = append(pools, s.Pool)
			if poolStack == nil && (poolName == "" || s.Pool == poolName) {
				poolStack = &d.Stacks[i]
			}
		case render.KindMgmt:
			mgmtStack = &d.Stacks[i]
		}
	}
	if poolStack == nil {
		return dash.Config{}, ui.Errf(ui.ExitUsage, fmt.Sprintf("pools: %v", pools), "no pool %q in the config", poolName)
	}
	metrics := false
	for _, p := range d.Cfg.Pools {
		if p.Name == poolStack.Pool {
			metrics = p.EnableMetrics
		}
	}

	st := ui.Begin(d.Cfg.Name, "connecting", 0)
	defer st.End("", false)

	poolNodes := func(ctx context.Context) ([]awsx.Node, error) { return a.stackNodes(ctx, d, *poolStack) }
	nodes, err := poolNodes(ctx)
	if err != nil {
		return dash.Config{}, err
	}
	gwNode := firstOnline(nodes)
	if gwNode == "" {
		return dash.Config{}, ui.Errf(ui.ExitAWS, "check `bbctl status "+d.Cfg.Name+"`; nodes need outbound access to SSM through the NAT gateway",
			"no node of %s is online in SSM", poolStack.ID)
	}
	cfg := dash.Config{
		Deployment: d.Cfg.Name, Region: d.Cfg.Region, Cluster: d.Cfg.Cluster,
		Pool: poolStack.Pool, Pools: pools, Nodes: poolNodes,
	}
	if cfg.Gateway, err = a.forward(ctx, d, gwNode, gatewayPort); err != nil {
		return dash.Config{}, err
	}

	promNode := ""
	if d.Cfg.Mgmt != nil && d.Cfg.Mgmt.EnableMetrics && mgmtStack != nil {
		if mn, err := a.stackNodes(ctx, d, *mgmtStack); err == nil {
			if promNode = firstOnline(mn); promNode != "" {
				cfg.PoolLabel = poolStack.Pool
			}
		}
	}
	if promNode == "" && metrics {
		promNode = gwNode
	}
	if promNode != "" {
		addr, err := a.forward(ctx, d, promNode, prometheusPort)
		if err != nil {
			return dash.Config{}, err
		}
		cfg.Prometheus = "http://" + addr
	}
	return cfg, nil
}

func (a *app) stackNodes(ctx context.Context, d *deploy.Deployment, s render.Stack) ([]awsx.Node, error) {
	out, err := d.Outputs(ctx, s)
	if err != nil {
		return nil, err
	}
	ids := deploy.InstanceIDs(out)
	if len(ids) == 0 {
		return nil, ui.Errf(ui.ExitUsage, "run `bbctl apply "+d.Cfg.Name+"`", "%s has no nodes yet", s.ID)
	}
	nodes, err := d.AWS.Nodes(ctx, ids)
	if err != nil {
		return nil, ui.Wrap(ui.ExitAWS, "", err)
	}
	return nodes, nil
}

func firstOnline(nodes []awsx.Node) string {
	for _, n := range nodes {
		if n.SSM == "Online" {
			return n.InstanceID
		}
	}
	return ""
}

// forward keeps a local port forwarded to remotePort on the instance for as
// long as ctx lives, and returns its host:port once it accepts connections.
//
// The session runs in a child `bbctl connect --forward`: the Session Manager
// code ends its process when a session closes, so it cannot share bbctl's.
// A session that drops (SSM ends idle ones) is started again.
func (a *app) forward(ctx context.Context, d *deploy.Deployment, instance string, remotePort int) (string, error) {
	local, err := freePort()
	if err != nil {
		return "", err
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	args := []string{"connect", d.Cfg.Name, "--node", instance, "--forward", fmt.Sprintf("%d:%d", local, remotePort)}
	if a.profile != "" {
		args = append(args, "--profile", a.profile)
	}
	exited := make(chan error, 1)
	go func() {
		first := true
		for ctx.Err() == nil {
			c := exec.CommandContext(ctx, self, args...)
			if ui.Verbose {
				c.Stdout, c.Stderr = os.Stderr, os.Stderr
			}
			err := c.Run()
			if first {
				exited <- err
				first = false
			}
			ui.Logf("forward to %s:%d ended (%v), restarting", instance, remotePort, err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(local))
	deadline := time.After(60 * time.Second)
	for {
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			ui.Logf("forwarding %s -> %s:%d", addr, instance, remotePort)
			return addr, nil
		}
		select {
		case err := <-exited:
			return "", ui.Errf(ui.ExitAWS, "check `bbctl status "+d.Cfg.Name+"` and run with -v to see the session's output",
				"port forward to %s:%d ended: %v", instance, remotePort, err)
		case <-deadline:
			return "", ui.Errf(ui.ExitAWS, "run with -v to see the session's output", "port forward to %s:%d did not open in 60s", instance, remotePort)
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func openBrowser(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	_ = c.Start()
}
