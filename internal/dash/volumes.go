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

// The Volumes view: the mgx-metrics "Storage" Grafana dashboard. The storage
// plugin publishes each volume's record as labels on storage_volume_status,
// valued 1 when the volume is READY. The query is the dashboard's: topk by
// name keeps only the newest label set, since a changed label (a new status,
// a move to another node) starts a new series.
const volumesQuery = `topk(1, storage_volume_status{%[1]s} and ignoring(__name__) (timestamp(storage_volume_status{%[1]s}) == on(name) group_left() max by(name) (timestamp(storage_volume_status{%[1]s})))) by (name)`

// StorageView is the Volumes tab's summary and table.
type StorageView struct {
	Summary StorageSummary `json:"summary"`
	Volumes []Volume       `json:"volumes"`
	// Warnings are sources that failed; the rest of the view still shows.
	Warnings []string `json:"warnings,omitempty"`
}

type StorageSummary struct {
	Volumes  int `json:"volumes"`
	NotReady int `json:"not_ready"`
	// Sizes in MiB, summed over the volumes.
	SizeMiB float64 `json:"size_mib"`
	RWMiB   float64 `json:"rw_mib"`
	RMiB    float64 `json:"r_mib"`
}

// Volume is one row of the volume table. Sizes are in MiB, as the storage
// plugin reports them.
type Volume struct {
	Pool    string  `json:"pool"`
	Name    string  `json:"name"`
	Status  string  `json:"status"`
	Ready   bool    `json:"ready"`
	SizeMiB float64 `json:"size_mib"`
	RWMiB   float64 `json:"rw_mib"`
	RMiB    float64 `json:"r_mib"`
	Bucket  string  `json:"bucket,omitempty"`
	// Node is the UUID of the node serving the volume, Hostname its name.
	Node     string `json:"node,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	IP       string `json:"ip,omitempty"`
	// QoS are the volume's caps, as the storage plugin labels them (MB/s and
	// IOPS); a cap it leaves unset is missing.
	QoS map[string]string `json:"qos,omitempty"`
	// ExtraParams are the nbdkit arguments added on volume start.
	ExtraParams string `json:"extra_params,omitempty"`
}

// qosLabels are the status labels that hold a volume's QoS caps.
var qosLabels = []string{"qos_r_mbytes_per_sec", "qos_w_mbytes_per_sec", "qos_rw_mbytes_per_sec", "qos_rw_ios_per_sec"}

// labelValue is a label the plugin set: it writes Python's None as "None".
func labelValue(v string) string {
	if v == "None" || v == "null" {
		return ""
	}
	return strings.TrimSpace(v)
}

func (s *Server) storage(w http.ResponseWriter, r *http.Request) {
	pools, err := s.selected(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	vols := make([][]Volume, len(pools))
	errs := make([]error, len(pools))
	each(pools, func(i int, p *pool) { vols[i], errs[i] = p.volumes(ctx) })

	v := StorageView{Volumes: []Volume{}}
	for i, p := range pools {
		if errs[i] != nil {
			v.Warnings = append(v.Warnings, s.warn(p, errs[i]))
		}
		v.Volumes = append(v.Volumes, vols[i]...)
	}
	for _, vol := range v.Volumes {
		v.Summary.Volumes++
		if !vol.Ready {
			v.Summary.NotReady++
		}
		v.Summary.SizeMiB += vol.SizeMiB
		v.Summary.RWMiB += vol.RWMiB
		v.Summary.RMiB += vol.RMiB
	}
	writeJSON(w, http.StatusOK, v)
}

// volumes is one pool's volume rows, ordered by name.
func (p *pool) volumes(ctx context.Context) ([]Volume, error) {
	if p.prom == nil {
		return nil, errNoMetrics
	}
	var (
		wg        sync.WaitGroup
		res       []sample
		hostnames map[string]string
		errs      [2]error
	)
	wg.Add(2)
	go func() { defer wg.Done(); res, errs[0] = p.prom.query(ctx, fmt.Sprintf(volumesQuery, p.selector())) }()
	go func() { defer wg.Done(); hostnames, errs[1] = p.hostnames(ctx) }()
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}
	out := make([]Volume, 0, len(res))
	for _, s := range res {
		l := s.Labels
		v := Volume{
			Pool: p.name, Name: l["name"], Status: l["status"], Ready: s.Value == 1,
			Bucket: l["storage_bucket"], Node: l["sc_node"], Hostname: hostnames[l["sc_node"]], IP: l["ip"],
		}
		v.SizeMiB, _ = strconv.ParseFloat(l["size"], 64)
		v.RWMiB, _ = strconv.ParseFloat(l["cache_rw_cache_size"], 64)
		v.RMiB, _ = strconv.ParseFloat(l["cache_r_cache_size"], 64)
		for _, k := range qosLabels {
			if q := labelValue(l[k]); q != "" {
				if v.QoS == nil {
					v.QoS = map[string]string{}
				}
				v.QoS[strings.TrimPrefix(k, "qos_")] = q
			}
		}
		v.ExtraParams = labelValue(l["extra_params"])
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// volumePanels are the dashboard's Cache, SPDK and S3 rows, one line per
// volume (and per node it was served from). Sizes the dashboard shows in MiB
// are bytes here, for the page to scale. The SPDK counters are per-scrape
// deltas, so dividing by the 15s scrape interval gives a rate; summing by
// name keeps a volume that moved to another node one line.
var volumePanels = map[string]panel{
	// Cache
	"cgroup_memory": {"bytes", []line{
		{`cgroup_fs_cache_bytes{{sel}}`, "{{name}} fs_cache"},
		{`cgroup_anon_bytes{{sel}}`, "{{name}} anon"},
		{`cgroup_mem_max_bytes{{sel}}`, "{{name}} max"},
		{`cgroup_mem_low_bytes{{sel}}`, "{{name}} low"},
		{`cgroup_mem_high_bytes{{sel}}`, "{{name}} high"},
	}},
	"cgroup_memory_dirty": {"bytes", []line{
		{`cgroup_dirty_bytes{{sel}}`, "{{name}} dirty"},
		{`cgroup_writeback_bytes{{sel}}`, "{{name}} writeback"},
	}},
	"cache_write_allocated": {"bytes", []line{
		{`cache_write_allocated_bytes{{sel}}`, "{{name}}"},
		{`cache_rw_cache_size{{sel}} * 1048576`, "{{name}} max"},
	}},
	"cache_read_allocated": {"bytes", []line{
		{`cache_read_allocated_bytes{{sel}}`, "{{name}}"},
		{`cache_r_cache_size{{sel}} * 1048576`, "{{name}} max"},
	}},
	"memory_rss":           {"bytes", []line{{`memory_rss_bytes{{sel}}`, "{{name}}"}}},
	"threads_running":      {"count", []line{{`threads_running{{sel}}`, "{{name}}"}}},
	"threads_iowait":       {"count", []line{{`threads_iowait{{sel}}`, "{{name}}"}}},
	"block_read_hits":      {"count", []line{{`block_cache_read_hits{{sel}}`, "{{name}}"}}},
	"block_read_misses":    {"count", []line{{`block_cache_read_misses{{sel}}`, "{{name}}"}}},
	"block_read_hit_ratio": {"count", []line{{`block_cache_read_hit_ratio{{sel}}`, "{{name}}"}}},
	"block_write_hits":     {"count", []line{{`block_cache_write_hits{{sel}}`, "{{name}}"}}},
	"block_write_misses":   {"count", []line{{`block_cache_write_misses{{sel}}`, "{{name}}"}}},
	"block_write_hit_ratio": {"count", []line{
		{`block_cache_write_hit_ratio{{sel}}`, "{{name}}"},
	}},
	"cache_evictions": {"count", []line{
		{`cache_evict_write_side{{sel}}`, "{{name}} write_side"},
		{`cache_evict_lru{{sel}}`, "{{name}} lru"},
		{`cache_evict_fallback{{sel}}`, "{{name}} fallback"},
	}},
	"cache_reclaim_scan": {"count", []line{
		{`cache_reclaim_scan_blocks{{sel}}`, "{{name}} blocks"},
		{`cache_reclaim_scan_misses{{sel}}`, "{{name}} misses"},
		{`cache_reclaim_scan_giveups{{sel}}`, "{{name}} giveups"},
	}},
	"cache_reclaim_stalls": {"count", []line{{`cache_reclaim_stalls{{sel}}`, "{{name}}"}}},
	"cache_readahead": {"count", []line{
		{`cache_readahead_arms{{sel}}`, "{{name}} arms"},
		{`cache_readahead_batches{{sel}}`, "{{name}} batches"},
	}},
	"cache_readahead_blocks": {"count", []line{
		{`cache_readahead_blocks{{sel}}`, "{{name}} fetched"},
		{`cache_readahead_present{{sel}}`, "{{name}} present"},
		{`cache_readahead_raced{{sel}}`, "{{name}} raced"},
	}},
	"cache_cached": {"bytes", []line{
		{`cache_read_cached_bytes{{sel}}`, "{{name}} read_cached"},
		{`cache_present_bytes{{sel}}`, "{{name}} present"},
		{`cache_clean_write_side_bytes{{sel}}`, "{{name}} clean_write_side"},
	}},
	"cache_read_hits":   {"count", []line{{`cache_read_hits{{sel}}`, "{{name}}"}}},
	"cache_read_misses": {"count", []line{{`cache_read_misses{{sel}}`, "{{name}}"}}},

	// SPDK
	"spdk_read":       {"bytes_rate", []line{{`sum by(name) (spdk_volume_bytes_read{{sel}}) / 15`, "{{name}}"}}},
	"spdk_write":      {"bytes_rate", []line{{`sum by(name) (spdk_volume_bytes_written{{sel}}) / 15`, "{{name}}"}}},
	"spdk_read_iops":  {"ops", []line{{`sum by(name) (spdk_volume_num_read_ops{{sel}}) / 15`, "{{name}}"}}},
	"spdk_write_iops": {"ops", []line{{`sum by(name) (spdk_volume_num_write_ops{{sel}}) / 15`, "{{name}}"}}},
	"spdk_read_latency": {"count", []line{
		{`spdk_volume_min_read_latency_ticks{{sel}}`, "{{name}} min"},
		{`spdk_volume_max_read_latency_ticks{{sel}}`, "{{name}} max"},
	}},
	"spdk_write_latency": {"count", []line{
		{`spdk_volume_min_write_latency_ticks{{sel}}`, "{{name}} min"},
		{`spdk_volume_max_write_latency_ticks{{sel}}`, "{{name}} max"},
	}},

	// S3
	"http_gets":            {"count", []line{{`http_gets{{sel}}`, "{{name}}"}}},
	"http_avg_get_time":    {"count", []line{{`http_avg_get_time{{sel}}`, "{{name}}"}}},
	"http_puts":            {"count", []line{{`http_puts{{sel}}`, "{{name}}"}}},
	"http_avg_put_time":    {"count", []line{{`http_avg_put_time{{sel}}`, "{{name}}"}}},
	"http_deletes":         {"count", []line{{`http_deletes{{sel}}`, "{{name}}"}}},
	"http_avg_delete_time": {"count", []line{{`http_avg_delete_time{{sel}}`, "{{name}}"}}},
	"http_errors": {"count", []line{
		{`http_5xx_error{{sel}}`, "{{name}} 5xx"},
		{`http_4xx_error{{sel}}`, "{{name}} 4xx"},
		{`http_3xx_error{{sel}}`, "{{name}} 3xx"},
		{`http_other_error{{sel}}`, "{{name}} other"},
	}},
}

// volumeSeries is one panel of the selected pools' volumes.
func (s *Server) volumeSeries(w http.ResponseWriter, r *http.Request) {
	pools, err := s.selected(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	pn, ok := volumePanels[r.URL.Query().Get("panel")]
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown panel"))
		return
	}
	sels := make([]string, len(pools))
	for i, p := range pools {
		sels[i] = p.selector()
	}
	out, errs := drawPanel(r, pn, pools, sels, true)
	for i, p := range pools {
		if errs[i] != nil {
			out.Warnings = append(out.Warnings, s.warn(p, errs[i]))
		}
	}
	writeJSON(w, http.StatusOK, out)
}
