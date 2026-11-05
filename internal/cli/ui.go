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
	"strings"
	"sync"
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
		Short: "open a dashboard of the storage's pools in the browser",
		Long: `Open a dashboard of the storage's pools in the browser: the data of the
mgx-metrics Grafana dashboards, read from the clusters' Prometheus. bbctl
reaches it over SSM port forwards and serves the page on 127.0.0.1 until
Ctrl-C.`,
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
			cfg, pools, err := a.uiConfig(ctx, d, pool)
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
			if err := ui.Result(map[string]any{"url": url, "pools": pools}, func(w io.Writer) {
				fmt.Fprintf(w, "dashboard for %s (%s) at %s (Ctrl-C to stop)\n", d.Cfg.Name, strings.Join(pools, ", "), url)
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
	f.StringVar(&pool, "pool", "", "show only this pool (default: every pool)")
	f.IntVar(&port, "port", 8080, "local port for the dashboard")
	f.BoolVar(&noOpen, "no-open", false, "do not open a browser")
	return cmd
}

// uiConfig finds the nodes to read from and starts the port forwards to
// them; AWS is used for nothing else. Every applied pool is shown (or only
// --pool). Prometheus is the management plane's when there is one: it
// federates every pool's series, labelled by pool. Otherwise each pool has
// its own. The cluster API, used only to check logins, is the first pool's.
func (a *app) uiConfig(ctx context.Context, d *deploy.Deployment, only string) (dash.Config, []string, error) {
	var pools []render.Stack
	var mgmtStack *render.Stack
	var names []string
	for i, s := range d.Stacks {
		switch s.Kind {
		case render.KindPool:
			names = append(names, s.Pool)
			if only == "" || s.Pool == only {
				pools = append(pools, s)
			}
		case render.KindMgmt:
			mgmtStack = &d.Stacks[i]
		}
	}
	if len(pools) == 0 {
		return dash.Config{}, nil, ui.Errf(ui.ExitUsage, fmt.Sprintf("pools: %v", names), "no pool %q in the config", only)
	}
	metrics := map[string]bool{}
	for _, p := range d.Cfg.Pools {
		metrics[p.Name] = p.EnableMetrics
	}

	st := ui.Begin(d.Cfg.Name, "connecting", 0)
	defer st.End("", false)

	// The pools' nodes; a pool that is not applied yet is left out.
	online := map[string]string{}
	var shown []string
	for _, s := range pools {
		nodes, err := a.stackNodes(ctx, d, s)
		if err != nil {
			ui.Logf("%s: %v", s.ID, err)
			continue
		}
		if id := firstOnline(nodes); id != "" {
			online[s.Pool] = id
			shown = append(shown, s.Pool)
		}
	}
	if len(shown) == 0 {
		return dash.Config{}, nil, ui.Errf(ui.ExitAWS, "check `bbctl status "+d.Cfg.Name+"`; nodes need outbound access to SSM through the NAT gateway",
			"no pool has a node online in SSM")
	}
	mgmtNode := ""
	if d.Cfg.Mgmt != nil && d.Cfg.Mgmt.EnableMetrics && mgmtStack != nil {
		if mn, err := a.stackNodes(ctx, d, *mgmtStack); err == nil {
			mgmtNode = firstOnline(mn)
		}
	}

	// The forwards, opened together: the cluster API, then one Prometheus
	// per pool, or mgmt's for all of them.
	type fwd struct {
		instance string
		port     int
		addr     string
		err      error
	}
	fwds := []*fwd{{instance: online[shown[0]], port: gatewayPort}}
	promFwd := map[string]*fwd{}
	for _, p := range shown {
		switch {
		case mgmtNode != "":
			if len(promFwd) == 0 {
				fwds = append(fwds, &fwd{instance: mgmtNode, port: prometheusPort})
			}
			promFwd[p] = fwds[1]
		case metrics[p]:
			f := &fwd{instance: online[p], port: prometheusPort}
			fwds = append(fwds, f)
			promFwd[p] = f
		}
	}
	var wg sync.WaitGroup
	for _, f := range fwds {
		wg.Add(1)
		go func() { defer wg.Done(); f.addr, f.err = a.forward(ctx, d, f.instance, f.port) }()
	}
	wg.Wait()
	for _, f := range fwds {
		if f.err != nil {
			return dash.Config{}, nil, f.err
		}
	}

	cfg := dash.Config{
		Cluster:   d.Cfg.Cluster,
		Gateway:   fwds[0].addr,
		LoginHint: "bbctl secrets get <config> " + scPasswordKey,
	}
	for _, p := range shown {
		dp := dash.Pool{Name: p}
		if f := promFwd[p]; f != nil {
			dp.Prometheus = "http://" + f.addr
			if mgmtNode != "" {
				dp.Label = p
			}
		}
		cfg.Pools = append(cfg.Pools, dp)
	}
	return cfg, shown, nil
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
	args := []string{"connect", d.Cfg.Name, "--forward", fmt.Sprintf("%d:%d", local, remotePort)}
	if a.profile != "" {
		args = append(args, "--profile", a.profile)
	}
	exited := make(chan error, 1)
	go func() {
		first := true
		for ctx.Err() == nil {
			c := exec.CommandContext(ctx, self, args...)
			// The instance is already known: the child goes straight to the
			// session, without Terraform, whose working directories another
			// bbctl may hold.
			c.Env = append(os.Environ(), forwardInstanceEnv+"="+instance)
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
