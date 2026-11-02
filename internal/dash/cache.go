package dash

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The Cache view: the mgx-metrics "Cache" Grafana dashboard. The cache
// plugin reports its state as labels on three series, where "1" means OK and
// "0" means down. The queries are the dashboard's: topk by the row's key
// keeps only the newest label set, since a changed label starts a new series.
const (
	cacheNodesQuery = `topk(1, cache_node_status{%[1]s} and ignoring(__name__) (timestamp(cache_node_status{%[1]s}) == on(ip) group_left() max by(ip) (timestamp(cache_node_status{%[1]s})))) by (ip)`
	cacheRaidQuery  = `topk(1, cache_raid_member_state{%[1]s} and ignoring(__name__) (timestamp(cache_raid_member_state{%[1]s}) == on(ip, raid, raid_device) group_left() max by(ip, raid, raid_device) (timestamp(cache_raid_member_state{%[1]s})))) by (ip, raid, raid_device)`
	cacheMountQuery = `topk(1, cache_nvme_mounts{%[1]s} and ignoring(__name__) (timestamp(cache_nvme_mounts{%[1]s}) == on(mountpoint) group_left() max by(mountpoint) (timestamp(cache_nvme_mounts{%[1]s})))) by (mountpoint)`
)

// CacheView is the Cache tab.
type CacheView struct {
	Summary CacheSummary `json:"summary"`
	Nodes   []CacheNode  `json:"nodes"`
	Raids   []CacheRaid  `json:"raids"`
	Mounts  []CacheMount `json:"mounts"`
	// Warnings are sources that failed; the rest of the view still shows.
	Warnings []string `json:"warnings,omitempty"`
}

type CacheSummary struct {
	NodesUp    int `json:"nodes_up"`
	NodesDown  int `json:"nodes_down"`
	RaidsDown  int `json:"raids_down"`
	MountsDown int `json:"mounts_down"`
	// Cache sizes in MiB, over every node: per-disk size times disks.
	RWMiB float64 `json:"rw_mib"`
	RMiB  float64 `json:"r_mib"`
	Disks int     `json:"disks"`
}

// CacheNode is a node's cache: its checks and sizes. A node counts as up
// when every check passes (the dashboard's "Cache nodes DOWN" rule).
type CacheNode struct {
	Pool     string  `json:"pool"`
	Node     string  `json:"node"` // UUID
	Hostname string  `json:"hostname,omitempty"`
	IP       string  `json:"ip"` // data-subnet IP
	Status   bool    `json:"status"`
	Mounts   bool    `json:"mounts"`
	Errors   bool    `json:"errors"` // true: the error check passes
	Disks    int     `json:"disks"`
	RWMiB    float64 `json:"rw_mib"`
	RMiB     float64 `json:"r_mib"`
}

func (n CacheNode) up() bool { return n.Status && n.Mounts && n.Errors }

// CacheRaid is a node's cache RAID and its member disks.
type CacheRaid struct {
	Pool     string       `json:"pool"`
	Node     string       `json:"node"`
	Hostname string       `json:"hostname,omitempty"`
	Raid     string       `json:"raid"`
	Level    string       `json:"level"`
	State    string       `json:"state"`
	StateOK  bool         `json:"state_ok"`
	NoFailed bool         `json:"no_failed"` // the failed-devices check passes
	Total    int          `json:"total_devices"`
	Members  []RaidMember `json:"members"`
	// OK is the dashboard's rule: false when any member reports a failed
	// check or a zero value.
	OK bool `json:"ok"`
}

type RaidMember struct {
	Device string `json:"device"`
	State  string `json:"state"`
	OK     bool   `json:"ok"`
}

// CacheMount is one cache filesystem (read-write or read) on a node.
type CacheMount struct {
	Pool       string `json:"pool"`
	Node       string `json:"node"`
	Hostname   string `json:"hostname,omitempty"`
	Mountpoint string `json:"mountpoint"`
	Kind       string `json:"kind"` // rw or r
	Device     string `json:"device"`
	Options    string `json:"options"`
	OK         bool   `json:"ok"`
}

// flag reads a status label: "0" is down, anything else (normally "1") is up.
func flag(l map[string]string, name string) bool { return l[name] != "0" }

func (s *Server) cache(w http.ResponseWriter, r *http.Request) {
	pools, err := s.selected(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	views := make([]CacheView, len(pools))
	errs := make([]error, len(pools))
	each(pools, func(i int, p *pool) { views[i], errs[i] = p.cache(ctx) })

	v := CacheView{Nodes: []CacheNode{}, Raids: []CacheRaid{}, Mounts: []CacheMount{}}
	for i, p := range pools {
		if errs[i] != nil {
			v.Warnings = append(v.Warnings, s.warn(p, errs[i]))
		}
		v.Nodes = append(v.Nodes, views[i].Nodes...)
		v.Raids = append(v.Raids, views[i].Raids...)
		v.Mounts = append(v.Mounts, views[i].Mounts...)
	}
	v.Summary = summarizeCache(v)
	writeJSON(w, http.StatusOK, v)
}

// cache is one pool's tables, each ordered by hostname so a node is easy to
// follow down the page.
func (p *pool) cache(ctx context.Context) (CacheView, error) {
	var v CacheView
	if p.prom == nil {
		return v, errNoMetrics
	}
	sel := p.selector()
	var (
		wg                   sync.WaitGroup
		nodes, raids, mounts []sample
		hostnames            map[string]string
		errs                 [4]error
	)
	wg.Add(4)
	go func() { defer wg.Done(); nodes, errs[0] = p.prom.query(ctx, fmt.Sprintf(cacheNodesQuery, sel)) }()
	go func() { defer wg.Done(); raids, errs[1] = p.prom.query(ctx, fmt.Sprintf(cacheRaidQuery, sel)) }()
	go func() { defer wg.Done(); mounts, errs[2] = p.prom.query(ctx, fmt.Sprintf(cacheMountQuery, sel)) }()
	go func() { defer wg.Done(); hostnames, errs[3] = p.hostnames(ctx) }()
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		return v, err
	}
	v.Nodes, v.Raids, v.Mounts = cacheNodes(nodes), cacheRaids(raids), cacheMounts(mounts)
	for i := range v.Nodes {
		v.Nodes[i].Pool, v.Nodes[i].Hostname = p.name, hostnames[v.Nodes[i].Node]
	}
	for i := range v.Raids {
		v.Raids[i].Pool, v.Raids[i].Hostname = p.name, hostnames[v.Raids[i].Node]
	}
	for i := range v.Mounts {
		v.Mounts[i].Pool, v.Mounts[i].Hostname = p.name, hostnames[v.Mounts[i].Node]
	}
	sort.SliceStable(v.Nodes, func(i, j int) bool { return v.Nodes[i].Hostname < v.Nodes[j].Hostname })
	sort.SliceStable(v.Raids, func(i, j int) bool { return v.Raids[i].Hostname < v.Raids[j].Hostname })
	sort.SliceStable(v.Mounts, func(i, j int) bool { return v.Mounts[i].Hostname < v.Mounts[j].Hostname })
	return v, nil
}

func cacheNodes(in []sample) []CacheNode {
	out := make([]CacheNode, 0, len(in))
	for _, s := range in {
		l := s.Labels
		n := CacheNode{
			Node: l["node"], IP: l["ip"],
			Status: s.Value != 0, Mounts: flag(l, "attached_mounts"), Errors: flag(l, "errors"),
		}
		n.Disks, _ = strconv.Atoi(l["node_disks_count"])
		rw, _ := strconv.ParseFloat(l["rw_cache_size_in_mib"], 64)
		rd, _ := strconv.ParseFloat(l["r_cache_size_in_mib"], 64)
		n.RWMiB, n.RMiB = rw*float64(n.Disks), rd*float64(n.Disks)
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

func cacheRaids(in []sample) []CacheRaid {
	byKey := map[string]*CacheRaid{}
	var keys []string
	for _, s := range in {
		l := s.Labels
		key := l["ip"] + "|" + l["raid"]
		r, ok := byKey[key]
		if !ok {
			r = &CacheRaid{
				Node: l["node"], Raid: l["raid"], Level: l["raid_level"],
				State: strings.TrimSpace(l["raid_state"]), StateOK: flag(l, "raid_state_status"),
				NoFailed: flag(l, "failed_devices"), OK: true,
			}
			r.Total, _ = strconv.Atoi(l["total_devices"])
			byKey[key] = r
			keys = append(keys, key)
		}
		// The RAID's checks are labels on every member's row; any member
		// reporting a failure fails the RAID.
		r.NoFailed = r.NoFailed && flag(l, "failed_devices")
		r.StateOK = r.StateOK && flag(l, "raid_state_status")
		if s.Value == 0 || !flag(l, "errors") || !flag(l, "attached_mounts") || !r.NoFailed || !r.StateOK {
			r.OK = false
		}
		state := strings.TrimSpace(l["device_state"])
		r.Members = append(r.Members, RaidMember{Device: l["raid_device"], State: state, OK: s.Value != 0 && state == "active sync"})
	}
	sort.Strings(keys)
	out := make([]CacheRaid, 0, len(keys))
	for _, k := range keys {
		r := byKey[k]
		sort.Slice(r.Members, func(i, j int) bool { return deviceLess(r.Members[i].Device, r.Members[j].Device) })
		out = append(out, *r)
	}
	return out
}

// deviceLess orders /dev/nvme2n1 before /dev/nvme10n1.
func deviceLess(a, b string) bool {
	na, nb := firstNumber(a), firstNumber(b)
	if na != nb {
		return na < nb
	}
	return a < b
}

func firstNumber(s string) int {
	i := strings.IndexAny(s, "0123456789")
	if i < 0 {
		return -1
	}
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	n, _ := strconv.Atoi(s[i:j])
	return n
}

func cacheMounts(in []sample) []CacheMount {
	out := make([]CacheMount, 0, len(in))
	for _, s := range in {
		l := s.Labels
		m := CacheMount{
			Node: l["node"], Mountpoint: l["mountpoint"], Device: l["device"], Options: l["opts"],
			OK: s.Value != 0 && flag(l, "nvme_mounts_status") && flag(l, "attached_mounts") && flag(l, "errors"),
		}
		// Mountpoints are /mnt/s3cache/<rw|r>/<node uuid>.
		if parts := strings.Split(strings.Trim(m.Mountpoint, "/"), "/"); len(parts) >= 3 {
			m.Kind = parts[len(parts)-2]
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Kind > out[j].Kind // rw before r
	})
	return out
}

func summarizeCache(v CacheView) CacheSummary {
	var s CacheSummary
	for _, n := range v.Nodes {
		if n.up() {
			s.NodesUp++
		} else {
			s.NodesDown++
		}
		s.RWMiB += n.RWMiB
		s.RMiB += n.RMiB
		s.Disks += n.Disks
	}
	for _, r := range v.Raids {
		if !r.OK {
			s.RaidsDown++
		}
	}
	// Counted by node and device, as the dashboard does: the read-write and
	// read caches of a node share one RAID device.
	down := map[string]bool{}
	for _, m := range v.Mounts {
		if !m.OK {
			down[m.Pool+"|"+m.Node+"|"+m.Device] = true
		}
	}
	s.MountsDown = len(down)
	return s
}
