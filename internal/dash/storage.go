package dash

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// The Storage view: the mgx-metrics "Storage" Grafana dashboard. The storage
// plugin publishes each volume's record as labels on storage_volume_status,
// valued 1 when the volume is READY. The query is the dashboard's: topk by
// name keeps only the newest label set, since a changed label (a new status,
// a move to another node) starts a new series.
const volumesQuery = `topk(1, storage_volume_status{%[1]s} and ignoring(__name__) (timestamp(storage_volume_status{%[1]s}) == on(name) group_left() max by(name) (timestamp(storage_volume_status{%[1]s})))) by (name)`

// StorageView is the Storage tab.
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
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
