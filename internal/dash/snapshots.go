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

// The Snapshots view: the mgx-metrics "Snapshot" Grafana dashboard. The
// snapshot plugin publishes each snapshot's record as labels on
// snapshot_status, and its transfer stats as one series each. As for
// volumes, topk keeps only a snapshot's newest label set.
const snapshotsQuery = `topk(1, snapshot_status{%[1]s} and ignoring(__name__) (timestamp(snapshot_status{%[1]s}) == on(name, storage_pool) group_left() max by(name, storage_pool) (timestamp(snapshot_status{%[1]s})))) by (name, storage_pool)`

// snapshotStats are the stat panels' series, summed and counted per metric
// in one query each, so the averages hold across pools.
const snapshotStats = `__name__=~"snapshot_(size|errors|fatal_error|retry_error|server_side_copy_bytes|bytes|elapsed_time|transfer_time|total_transfers|speed)"`

// SnapshotsView is the Snapshots tab.
type SnapshotsView struct {
	Summary   SnapshotSummary `json:"summary"`
	Snapshots []Snapshot      `json:"snapshots"`
	// Warnings are sources that failed; the rest of the view still shows.
	Warnings []string `json:"warnings,omitempty"`
}

// SnapshotSummary is the dashboard's stat panels. Sizes are in bytes (the
// plugin reports the logical size in MB), times in seconds, speed in bytes
// per second. The averages are over the snapshots reporting each.
type SnapshotSummary struct {
	Snapshots         int      `json:"snapshots"`
	Ready             int      `json:"ready"`
	NotReady          int      `json:"not_ready"`
	LogicalBytes      float64  `json:"logical_bytes"`
	TransferErrors    float64  `json:"transfer_errors"`
	FatalErrors       float64  `json:"fatal_errors"`
	RetryErrors       float64  `json:"retry_errors"`
	ServerSideBytes   float64  `json:"server_side_bytes"`
	ServerSideShare   *float64 `json:"server_side_share,omitempty"`
	AvgElapsed        *float64 `json:"avg_elapsed,omitempty"`
	AvgTransferTime   *float64 `json:"avg_transfer_time,omitempty"`
	AvgTotalTransfers *float64 `json:"avg_total_transfers,omitempty"`
	AvgSpeed          *float64 `json:"avg_speed,omitempty"`
}

// Snapshot is one row of the snapshot table.
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
}

// snapshotTotals is one pool's sum and count of each stat series, by metric.
type snapshotTotals struct{ sum, count map[string]float64 }

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	pools, err := s.selected(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	rows := make([][]Snapshot, len(pools))
	totals := make([]snapshotTotals, len(pools))
	errs := make([]error, len(pools))
	each(pools, func(i int, p *pool) { rows[i], totals[i], errs[i] = p.snapshots(ctx) })

	v := SnapshotsView{Snapshots: []Snapshot{}}
	sum, count := map[string]float64{}, map[string]float64{}
	for i, p := range pools {
		if errs[i] != nil {
			v.Warnings = append(v.Warnings, s.warn(p, errs[i]))
			continue
		}
		v.Snapshots = append(v.Snapshots, rows[i]...)
		for k, x := range totals[i].sum {
			sum[k] += x
		}
		for k, x := range totals[i].count {
			count[k] += x
		}
	}
	m := &v.Summary
	for _, sn := range v.Snapshots {
		m.Snapshots++
		if sn.Ready {
			m.Ready++
		} else {
			m.NotReady++
		}
	}
	m.LogicalBytes = sum["snapshot_size"] * 1e6
	m.TransferErrors, m.FatalErrors, m.RetryErrors = sum["snapshot_errors"], sum["snapshot_fatal_error"], sum["snapshot_retry_error"]
	m.ServerSideBytes = sum["snapshot_server_side_copy_bytes"]
	if b := sum["snapshot_bytes"]; b > 0 {
		share := 100 * m.ServerSideBytes / b
		m.ServerSideShare = &share
	}
	avg := func(metric string) *float64 {
		if count[metric] == 0 {
			return nil
		}
		a := sum[metric] / count[metric]
		return &a
	}
	m.AvgElapsed, m.AvgTransferTime = avg("snapshot_elapsed_time"), avg("snapshot_transfer_time")
	m.AvgTotalTransfers, m.AvgSpeed = avg("snapshot_total_transfers"), avg("snapshot_speed")
	writeJSON(w, http.StatusOK, v)
}

// snapshots is one pool's snapshot rows, ordered by name, and its stat
// totals.
func (p *pool) snapshots(ctx context.Context) ([]Snapshot, snapshotTotals, error) {
	t := snapshotTotals{map[string]float64{}, map[string]float64{}}
	if p.prom == nil {
		return nil, t, errNoMetrics
	}
	sel := p.selector()
	stats := "{" + matchers(snapshotStats, sel) + "}"
	var (
		wg                sync.WaitGroup
		res, sums, counts []sample
		hostnames         map[string]string
		errs              [4]error
	)
	wg.Add(4)
	go func() { defer wg.Done(); res, errs[0] = p.prom.query(ctx, fmt.Sprintf(snapshotsQuery, sel)) }()
	go func() { defer wg.Done(); sums, errs[1] = p.prom.query(ctx, "sum by(__name__) ("+stats+")") }()
	go func() { defer wg.Done(); counts, errs[2] = p.prom.query(ctx, "count by(__name__) ("+stats+")") }()
	go func() { defer wg.Done(); hostnames, errs[3] = p.hostnames(ctx) }()
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		return nil, t, err
	}
	for _, x := range sums {
		t.sum[x.Labels["__name__"]] = x.Value
	}
	for _, x := range counts {
		t.count[x.Labels["__name__"]] = x.Value
	}
	out := make([]Snapshot, 0, len(res))
	for _, x := range res {
		l := x.Labels
		out = append(out, Snapshot{
			Pool: p.name, Name: l["name"], StoragePool: l["storage_pool"], Kind: l["kind"],
			Status: l["status"], Ready: l["status"] == "READY", Incremental: l["incremental"],
			Volume: l["volume"], Node: l["sc_node"], Hostname: hostnames[l["sc_node"]],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, t, nil
}
