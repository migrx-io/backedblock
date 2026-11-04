package dash

import (
	"context"
	"sort"
	"strings"
	"time"
)

// The Hosts view is the "Node Exporter Full" Grafana dashboard: one node at a
// time (hostpanels.go). Through the management plane node_exporter's series
// carry the pool and node=<ip>; a pool's own Prometheus scrapes <ip>:9100.
// scrapedHost reads either.

// expand fills in a query's pool matchers, `{{sel}}` alone in the braces or
// `{sel}` after others, and its rate window, {win}.
func expand(expr, sel string, win time.Duration) string {
	after := sel
	if sel != "" {
		after = "," + sel
	}
	return strings.NewReplacer("{{sel}}", "{"+sel+"}", "{sel}", after, "{win}", win.String()).Replace(expr)
}

// Host is a node that node_exporter reports, with its name and kernel.
type Host struct {
	Pool     string `json:"pool"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
}

// hosts is the pool's node_exporter targets, ordered by hostname.
func (p *pool) hosts(ctx context.Context) ([]Host, error) {
	if p.prom == nil {
		return nil, errNoMetrics
	}
	res, err := p.prom.query(ctx, "node_uname_info{"+p.selector()+"}")
	if err != nil {
		return nil, err
	}
	byIP := map[string]Host{}
	for _, s := range res {
		if ip := scrapedHost(s.Labels); ip != "" {
			byIP[ip] = Host{Pool: p.name, IP: ip, Hostname: s.Labels["nodename"], Kernel: s.Labels["release"]}
		}
	}
	out := make([]Host, 0, len(byIP))
	for _, h := range byIP {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hostname != out[j].Hostname {
			return out[i].Hostname < out[j].Hostname
		}
		return out[i].IP < out[j].IP
	})
	return out, nil
}
