package dash

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// The Hosts view: each node's operating system, from node_exporter (the
// "Node Exporter Full" Grafana dashboard). Through the management plane its
// series carry the pool and node=<ip>; a pool's own Prometheus scrapes
// <ip>:9100. scrapedHost reads either.

// Devices counted as disks and network: physical disks only, since the cache
// RAID's traffic is already counted on its members, and no loopback or
// virtual interfaces.
const (
	diskDevices = `device=~"nvme[0-9]+n[0-9]+|xvd[a-z]+|sd[a-z]+"`
	netDevices  = `device!~"lo|veth.*|docker.*|br-.*|virbr.*"`
)

// expand fills in a query's pool matchers, `{{sel}}` alone in the braces or
// `{sel}` after others, and its rate window, {win}.
func expand(expr, sel string, win time.Duration) string {
	after := sel
	if sel != "" {
		after = "," + sel
	}
	return strings.NewReplacer("{{sel}}", "{"+sel+"}", "{sel}", after, "{win}", win.String()).Replace(expr)
}

// hostQueries are the Hosts table's columns, each one value per host.
var hostQueries = map[string]string{
	"cpu":        `100 * (1 - avg by(instance, node) (rate(node_cpu_seconds_total{mode="idle"{sel}}[{win}])))`,
	"cpus":       `count by(instance, node) (node_cpu_seconds_total{mode="idle"{sel}})`,
	"ram":        `100 * (1 - node_memory_MemAvailable_bytes{{sel}} / node_memory_MemTotal_bytes{{sel}})`,
	"ram_total":  `node_memory_MemTotal_bytes{{sel}}`,
	"root":       `100 * (1 - node_filesystem_avail_bytes{mountpoint="/"{sel}} / node_filesystem_size_bytes{mountpoint="/"{sel}})`,
	"load1":      `node_load1{{sel}}`,
	"disk_read":  `sum by(instance, node) (rate(node_disk_read_bytes_total{` + diskDevices + `{sel}}[{win}]))`,
	"disk_write": `sum by(instance, node) (rate(node_disk_written_bytes_total{` + diskDevices + `{sel}}[{win}]))`,
	"net_rx":     `sum by(instance, node) (rate(node_network_receive_bytes_total{` + netDevices + `{sel}}[{win}]))`,
	"net_tx":     `sum by(instance, node) (rate(node_network_transmit_bytes_total{` + netDevices + `{sel}}[{win}]))`,
	"dirty":      `node_memory_Dirty_bytes{{sel}}`,
	"psi_cpu":    `100 * rate(node_pressure_cpu_waiting_seconds_total{{sel}}[{win}])`,
	"psi_memory": `100 * rate(node_pressure_memory_waiting_seconds_total{{sel}}[{win}])`,
	"psi_io":     `100 * rate(node_pressure_io_waiting_seconds_total{{sel}}[{win}])`,
	"uname":      `node_uname_info{{sel}}`,
}

// Host is a node's node_exporter readings. Rates are per second over the last
// minute; PSI is the share of time some task was stalled, in percent.
type Host struct {
	Pool     string `json:"pool"`
	IP       string `json:"ip"`
	Hostname string `json:"hostname,omitempty"`
	Kernel   string `json:"kernel,omitempty"`
	// Values maps each hostQueries column but uname to its value.
	Values map[string]float64 `json:"values"`
}

// hosts is the pool's host rows, ordered by hostname.
func (p *pool) hosts(ctx context.Context) ([]Host, error) {
	if p.prom == nil {
		return nil, errNoMetrics
	}
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		res  = map[string][]sample{}
		errs []error
	)
	for name, q := range hostQueries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := p.prom.query(ctx, expand(q, p.selector(), time.Minute))
			mu.Lock()
			defer mu.Unlock()
			res[name] = out
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	byIP := map[string]*Host{}
	row := func(l map[string]string) *Host {
		ip := scrapedHost(l)
		if ip == "" {
			return nil
		}
		h, ok := byIP[ip]
		if !ok {
			h = &Host{Pool: p.name, IP: ip, Values: map[string]float64{}}
			byIP[ip] = h
		}
		return h
	}
	for _, s := range res["uname"] {
		if h := row(s.Labels); h != nil {
			h.Hostname, h.Kernel = s.Labels["nodename"], s.Labels["release"]
		}
	}
	for name, samples := range res {
		if name == "uname" {
			continue
		}
		for _, s := range samples {
			if h := row(s.Labels); h != nil {
				h.Values[name] = s.Value
			}
		}
	}
	out := make([]Host, 0, len(byIP))
	for _, h := range byIP {
		out = append(out, *h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hostname != out[j].Hostname {
			return out[i].Hostname < out[j].Hostname
		}
		return out[i].IP < out[j].IP
	})
	return out, nil
}
