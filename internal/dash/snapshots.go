package dash

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// The Snapshots view: the mgx-metrics "Snapshot" dashboard. The
// snapshot plugin publishes each snapshot's record as labels on
// snapshot_status, and its transfer stats as one series each. As for
// volumes, topk keeps only a snapshot's newest label set.
const snapshotsQuery = `topk(1, snapshot_status{%[1]s} and ignoring(__name__) (timestamp(snapshot_status{%[1]s}) == on(name, storage_pool) group_left() max by(name, storage_pool) (timestamp(snapshot_status{%[1]s})))) by (name, storage_pool)`

// snapshotStats are each snapshot's transfer stats, one series per metric,
// labelled with the snapshot's name and storage pool.
const snapshotStats = `__name__=~"snapshot_(size|errors|fatal_error|retry_error|server_side_copy_bytes|bytes|elapsed_time|transfer_time|total_transfers|speed)"`

// SnapshotsView is the Snapshots tab.
type SnapshotsView struct {
	Summary   SnapshotSummary `json:"summary"`
	Snapshots []Snapshot      `json:"snapshots"`
	// Warnings are sources that failed; the rest of the view still shows.
	Warnings []string `json:"warnings,omitempty"`
}

// SnapshotSummary is the tab's tiles. The logical size is in bytes.
type SnapshotSummary struct {
	Snapshots    int     `json:"snapshots"`
	Ready        int     `json:"ready"`
	NotReady     int     `json:"not_ready"`
	LogicalBytes float64 `json:"logical_bytes"`
}

// Snapshot is one row of the snapshot table, with its transfer stats. Sizes
// are in bytes (the plugin reports the logical size in MiB, the volume's
// size), times in seconds, speed in bytes per second; a stat the plugin did
// not report is missing.
type Snapshot struct {
	Pool        string `json:"pool"`
	Name        string `json:"name"`
	StoragePool string `json:"storage_pool,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Status      string `json:"status"`
	Ready       bool   `json:"ready"`
	Incremental string `json:"incremental,omitempty"`
	Volume      string `json:"volume,omitempty"`
	// Node is the UUID of the node that took the snapshot, Hostname its name.
	Node     string `json:"node,omitempty"`
	Hostname string `json:"hostname,omitempty"`

	LogicalBytes    *float64 `json:"logical_bytes,omitempty"`
	Bytes           *float64 `json:"bytes,omitempty"`
	ServerSideBytes *float64 `json:"server_side_bytes,omitempty"`
	TransferErrors  *float64 `json:"transfer_errors,omitempty"`
	FatalErrors     *float64 `json:"fatal_errors,omitempty"`
	RetryErrors     *float64 `json:"retry_errors,omitempty"`
	Elapsed         *float64 `json:"elapsed,omitempty"`
	TransferTime    *float64 `json:"transfer_time,omitempty"`
	Transfers       *float64 `json:"transfers,omitempty"`
	Speed           *float64 `json:"speed,omitempty"`
}

// stat is the Snapshot field each stat metric fills.
func (sn *Snapshot) stat(metric string) **float64 {
	switch metric {
	case "snapshot_size":
		return &sn.LogicalBytes
	case "snapshot_bytes":
		return &sn.Bytes
	case "snapshot_server_side_copy_bytes":
		return &sn.ServerSideBytes
	case "snapshot_errors":
		return &sn.TransferErrors
	case "snapshot_fatal_error":
		return &sn.FatalErrors
	case "snapshot_retry_error":
		return &sn.RetryErrors
	case "snapshot_elapsed_time":
		return &sn.Elapsed
	case "snapshot_transfer_time":
		return &sn.TransferTime
	case "snapshot_total_transfers":
		return &sn.Transfers
	case "snapshot_speed":
		return &sn.Speed
	}
	return nil
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	pools, err := s.selected(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	rows := make([][]Snapshot, len(pools))
	errs := make([]error, len(pools))
	each(pools, func(i int, p *pool) { rows[i], errs[i] = p.snapshots(ctx) })

	v := SnapshotsView{Snapshots: []Snapshot{}}
	for i, p := range pools {
		if errs[i] != nil {
			v.Warnings = append(v.Warnings, s.warn(p, errs[i]))
			continue
		}
		v.Snapshots = append(v.Snapshots, rows[i]...)
	}
	m := &v.Summary
	for _, sn := range v.Snapshots {
		m.Snapshots++
		if sn.Ready {
			m.Ready++
		} else {
			m.NotReady++
		}
		if sn.LogicalBytes != nil {
			m.LogicalBytes += *sn.LogicalBytes
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// snapshots is one pool's snapshot rows, ordered by name, with their stats.
func (p *pool) snapshots(ctx context.Context) ([]Snapshot, error) {
	if p.prom == nil {
		return nil, errNoMetrics
	}
	sel := p.selector()
	var (
		wg         sync.WaitGroup
		res, stats []sample
		hostnames  map[string]string
		errs       [3]error
	)
	wg.Add(3)
	go func() { defer wg.Done(); res, errs[0] = p.prom.query(ctx, fmt.Sprintf(snapshotsQuery, sel)) }()
	go func() { defer wg.Done(); stats, errs[1] = p.prom.query(ctx, "{"+matchers(snapshotStats, sel)+"}") }()
	go func() { defer wg.Done(); hostnames, errs[2] = p.hostnames(ctx) }()
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}
	out := make([]Snapshot, 0, len(res))
	index := map[[2]string]int{}
	for _, x := range res {
		l := x.Labels
		index[[2]string{l["name"], l["storage_pool"]}] = len(out)
		out = append(out, Snapshot{
			Pool: p.name, Name: l["name"], StoragePool: l["storage_pool"], Kind: l["kind"],
			Status: l["status"], Ready: l["status"] == "READY", Incremental: l["incremental"],
			Volume: l["volume"], Node: l["sc_node"], Hostname: hostnames[l["sc_node"]],
		})
	}
	for _, x := range stats {
		i, ok := index[[2]string{x.Labels["name"], x.Labels["storage_pool"]}]
		if !ok {
			continue
		}
		if f := out[i].stat(x.Labels["__name__"]); f != nil {
			v := x.Value
			if x.Labels["__name__"] == "snapshot_size" {
				v *= 1 << 20
			}
			*f = &v
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
