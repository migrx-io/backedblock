package dash

import (
	"context"
	"errors"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The host detail: the rows of the "Node Exporter Full" Grafana dashboard
// (1860) for one node. Queries use {sel} and {{sel}} for the pool and host
// matchers and {win} for the rate window, as expand fills them. Ratios the
// dashboard shows as 0-1 are scaled to percent here.

// cores is the node's CPU count, for per-CPU averages.
const cores = `scalar(count(count(node_cpu_seconds_total{{sel}}) by (cpu)))`

// Devices in the per-device panels: whole disks and the cache RAID, not
// partitions or the nbd devices of volumes not in use.
const panelDisks = `device=~"[a-z]+|nvme[0-9]+n[0-9]+|md[0-9]+"`

// Interfaces in the network panels: no loopback or virtual ones.
const netDevices = `device!~"lo|veth.*|docker.*|br-.*|virbr.*"`

// Filesystems in the space panels: real ones, not tmpfs or snap images.
const panelFS = `fstype!~"rootfs|tmpfs|squashfs|overlay|ramfs"`

// quickQueries are the "Quick CPU / Mem / Disk" row, one value each.
var quickQueries = map[string]string{
	"psi_cpu":    `100 * rate(node_pressure_cpu_waiting_seconds_total{{sel}}[{win}])`,
	"psi_memory": `100 * rate(node_pressure_memory_waiting_seconds_total{{sel}}[{win}])`,
	"psi_io":     `100 * rate(node_pressure_io_waiting_seconds_total{{sel}}[{win}])`,
	"psi_irq":    `100 * rate(node_pressure_irq_stalled_seconds_total{{sel}}[{win}])`,
	"cpu_busy":   `100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"{sel}}[{win}])))`,
	"sys_load":   `node_load1{{sel}} * 100 / ` + cores,
	"ram_used":   `clamp_min((1 - node_memory_MemAvailable_bytes{{sel}} / node_memory_MemTotal_bytes{{sel}}) * 100, 0)`,
	"swap_used":  `(node_memory_SwapTotal_bytes{{sel}} - node_memory_SwapFree_bytes{{sel}}) / node_memory_SwapTotal_bytes{{sel}} * 100`,
	"root_used":  `100 * (node_filesystem_size_bytes{mountpoint="/",fstype!="rootfs"{sel}} - node_filesystem_avail_bytes{mountpoint="/",fstype!="rootfs"{sel}}) / node_filesystem_size_bytes{mountpoint="/",fstype!="rootfs"{sel}}`,
	"cpu_cores":  `count(count(node_cpu_seconds_total{{sel}}) by (cpu))`,
	"ram_total":  `node_memory_MemTotal_bytes{{sel}}`,
	"swap_total": `node_memory_SwapTotal_bytes{{sel}}`,
	"root_total": `node_filesystem_size_bytes{mountpoint="/",fstype!="rootfs"{sel}}`,
	"uptime":     `node_time_seconds{{sel}} - node_boot_time_seconds{{sel}}`,
}

// line is one query of a panel; legend may name labels as {{label}}.
type line struct{ expr, legend string }

// panel is a chart of one host. Unit tells the page how to print values:
// percent, bytes, bits (per second), bytes_rate, ops (per second), seconds or
// count.
type panel struct {
	unit  string
	lines []line
}

func cpuMode(mode string) string {
	return `100 * sum(rate(node_cpu_seconds_total{` + mode + `{sel}}[{win}])) / ` + cores
}

func mem(name string) string { return `node_memory_` + name + `_bytes{{sel}}` }

func diskRate(metric string) string {
	return `rate(node_disk_` + metric + `{` + panelDisks + `{sel}}[{win}])`
}

var hostPanels = map[string]panel{
	// Basic CPU / Mem / Net / Disk
	"cpu_basic": {"percent", []line{
		{cpuMode(`mode="system"`), "Busy system"},
		{cpuMode(`mode="user"`), "Busy user"},
		{cpuMode(`mode="iowait"`), "Busy iowait"},
		{cpuMode(`mode=~".*irq"`), "Busy IRQs"},
		{cpuMode(`mode!~"idle|user|system|iowait|irq|softirq"`), "Busy other"},
		{cpuMode(`mode="idle"`), "Idle"},
	}},
	"mem_basic": {"bytes", []line{
		{mem("MemTotal"), "Total"},
		{mem("MemTotal") + " - " + mem("MemFree") + " - (" + mem("Cached") + " + " + mem("Buffers") + " + " + mem("SReclaimable") + ")", "Used"},
		{mem("Cached") + " + " + mem("Buffers") + " + " + mem("SReclaimable"), "Cache + buffer"},
		{mem("MemFree"), "Free"},
		{mem("SwapTotal") + " - " + mem("SwapFree"), "Swap used"},
	}},
	"net_basic": {"bits", []line{
		{`8 * rate(node_network_receive_bytes_total{` + netDevices + `{sel}}[{win}])`, "{{device}} rx"},
		{`8 * rate(node_network_transmit_bytes_total{` + netDevices + `{sel}}[{win}])`, "{{device}} tx"},
	}},
	"disk_space_basic": {"percent", []line{
		{`100 * (node_filesystem_size_bytes{` + panelFS + `{sel}} - node_filesystem_avail_bytes{` + panelFS + `{sel}}) / node_filesystem_size_bytes{` + panelFS + `{sel}}`, "{{mountpoint}}"},
	}},

	// CPU / Memory / Net / Disk
	"cpu": {"percent", []line{
		{cpuMode(`mode="system"`), "System"},
		{cpuMode(`mode="user"`), "User"},
		{cpuMode(`mode="nice"`), "Nice"},
		{cpuMode(`mode="iowait"`), "Iowait"},
		{cpuMode(`mode="irq"`), "Irq"},
		{cpuMode(`mode="softirq"`), "Softirq"},
		{cpuMode(`mode="steal"`), "Steal"},
		{cpuMode(`mode="idle"`), "Idle"},
	}},
	"memory": {"bytes", []line{
		{mem("MemTotal") + " - " + mem("MemFree") + " - " + mem("Buffers") + " - " + mem("Cached") + " - " + mem("Slab") + " - " + mem("PageTables") + " - " + mem("SwapCached"), "Apps"},
		{mem("PageTables"), "PageTables"},
		{mem("SwapCached"), "SwapCache"},
		{mem("Slab"), "Slab"},
		{mem("Cached"), "Cache"},
		{mem("Buffers"), "Buffers"},
		{mem("MemFree"), "Unused"},
		{mem("SwapTotal") + " - " + mem("SwapFree"), "Swap"},
		{mem("HardwareCorrupted"), "Hardware corrupted"},
	}},
	"net_saturation": {"percent", []line{
		{`100 * rate(node_network_receive_bytes_total{` + netDevices + `{sel}}[{win}]) / ignoring(speed) node_network_speed_bytes{speed!="-1",` + netDevices + `{sel}}`, "{{device}} rx"},
		{`100 * rate(node_network_transmit_bytes_total{` + netDevices + `{sel}}[{win}]) / ignoring(speed) node_network_speed_bytes{speed!="-1",` + netDevices + `{sel}}`, "{{device}} tx"},
	}},
	"disk_iops": {"ops", []line{
		{diskRate("reads_completed_total"), "{{device}} read"},
		{diskRate("writes_completed_total"), "{{device}} write"},
	}},
	"disk_throughput": {"bytes_rate", []line{
		{diskRate("read_bytes_total"), "{{device}} read"},
		{diskRate("written_bytes_total"), "{{device}} write"},
	}},
	"fs_avail": {"bytes", []line{
		{`node_filesystem_avail_bytes{` + panelFS + `{sel}}`, "{{mountpoint}}"},
		{`node_filesystem_free_bytes{` + panelFS + `{sel}}`, "{{mountpoint}} free"},
		{`node_filesystem_size_bytes{` + panelFS + `{sel}}`, "{{mountpoint}} size"},
	}},
	"fs_used": {"bytes", []line{
		{`node_filesystem_size_bytes{` + panelFS + `{sel}} - node_filesystem_avail_bytes{` + panelFS + `{sel}}`, "{{mountpoint}}"},
	}},
	"disk_util": {"percent", []line{
		{`100 * ` + diskRate("io_time_seconds_total"), "{{device}}"},
	}},
	"psi": {"percent", []line{
		{`100 * rate(node_pressure_cpu_waiting_seconds_total{{sel}}[{win}])`, "CPU some"},
		{`100 * rate(node_pressure_memory_waiting_seconds_total{{sel}}[{win}])`, "Memory some"},
		{`100 * rate(node_pressure_memory_stalled_seconds_total{{sel}}[{win}])`, "Memory full"},
		{`100 * rate(node_pressure_io_waiting_seconds_total{{sel}}[{win}])`, "IO some"},
		{`100 * rate(node_pressure_io_stalled_seconds_total{{sel}}[{win}])`, "IO full"},
		{`100 * rate(node_pressure_irq_stalled_seconds_total{{sel}}[{win}])`, "IRQ full"},
	}},

	// Memory Meminfo
	"mem_committed": {"bytes", []line{
		{mem("Committed_AS"), "Committed_AS"},
		{mem("CommitLimit"), "CommitLimit"},
	}},
	"mem_dirty": {"bytes", []line{
		{mem("Dirty"), "Dirty"},
		{mem("Writeback"), "Writeback"},
		{mem("WritebackTmp"), "WritebackTmp"},
		{mem("NFS_Unstable"), "NFS unstable"},
	}},
	"mem_slab": {"bytes", []line{
		{mem("SUnreclaim"), "SUnreclaim"},
		{mem("SReclaimable"), "SReclaimable"},
	}},
	"mem_shared": {"bytes", []line{
		{mem("Mapped"), "Mapped"},
		{mem("Shmem"), "Shmem"},
		{mem("ShmemHugePages"), "ShmemHugePages"},
		{mem("ShmemPmdMapped"), "PMD mapped"},
	}},
	"mem_lru": {"percent", []line{
		{`100 * ` + mem("Inactive") + ` / ` + mem("MemTotal"), "Inactive"},
		{`100 * ` + mem("Active") + ` / ` + mem("MemTotal"), "Active"},
	}},
	"mem_lru_detail": {"bytes", []line{
		{mem("Inactive_file"), "Inactive_file"},
		{mem("Inactive_anon"), "Inactive_anon"},
		{mem("Active_file"), "Active_file"},
		{mem("Active_anon"), "Active_anon"},
	}},
	"mem_kernel": {"bytes", []line{
		{mem("KernelStack"), "KernelStack"},
		{mem("Percpu"), "PerCPU"},
		{mem("Bounce"), "Bounce"},
	}},
	"mem_vmalloc": {"bytes", []line{
		{mem("VmallocChunk"), "Free chunk"},
		{mem("VmallocTotal"), "Total"},
		{mem("VmallocUsed"), "Used"},
	}},
	"mem_anon": {"bytes", []line{
		{mem("AnonHugePages"), "AnonHugePages"},
		{mem("AnonPages"), "AnonPages"},
	}},
	"mem_unevictable": {"bytes", []line{
		{mem("Unevictable"), "Unevictable"},
		{mem("Mlocked"), "Mlocked"},
	}},
	"mem_directmap": {"bytes", []line{
		{mem("DirectMap1G"), "1G pages"},
		{mem("DirectMap2M"), "2M pages"},
		{mem("DirectMap4k"), "4K pages"},
	}},
	"mem_hugepages": {"bytes", []line{
		{`(node_memory_HugePages_Total{{sel}} - node_memory_HugePages_Free{{sel}}) * ` + mem("Hugepagesize"), "Used"},
		{`node_memory_HugePages_Rsvd{{sel}} * ` + mem("Hugepagesize"), "Reserved"},
		{`node_memory_HugePages_Surp{{sel}} * ` + mem("Hugepagesize"), "Surplus"},
		{`node_memory_HugePages_Total{{sel}} * ` + mem("Hugepagesize"), "Total"},
	}},

	// Storage Disk (its IOps and data panels are disk_iops and
	// disk_throughput above)
	"disk_wait": {"seconds", []line{
		{diskRate("read_time_seconds_total") + " / " + diskRate("reads_completed_total"), "{{device}} read"},
		{diskRate("write_time_seconds_total") + " / " + diskRate("writes_completed_total"), "{{device}} write"},
	}},
	"disk_queue": {"count", []line{
		{diskRate("io_time_weighted_seconds_total"), "{{device}}"},
	}},
	"disk_merged": {"ops", []line{
		{diskRate("reads_merged_total"), "{{device}} read"},
		{diskRate("writes_merged_total"), "{{device}} write"},
	}},
	"disk_time": {"percent", []line{
		{`100 * ` + diskRate("io_time_seconds_total"), "{{device}} IO"},
		{`100 * ` + diskRate("discard_time_seconds_total"), "{{device}} discard"},
		{`100 * ` + diskRate("flush_requests_time_seconds_total"), "{{device}} flush"},
	}},
	"disk_discards": {"ops", []line{
		{diskRate("discards_completed_total"), "{{device}} discards completed"},
		{diskRate("discards_merged_total"), "{{device}} discards merged"},
		{diskRate("flush_requests_total"), "{{device}} flush"},
	}},
	"disk_discarded_sectors": {"count", []line{
		{diskRate("discarded_sectors_total"), "{{device}}"},
	}},
	"disk_io_now": {"count", []line{
		{`node_disk_io_now{` + panelDisks + `{sel}}`, "{{device}}"},
	}},
}

// hostOf finds the pool and the host matcher a request names with ?pool= and
// ?ip=. The IP is parsed, so nothing but an address reaches the query.
func (s *Server) hostOf(r *http.Request) (*pool, string, error) {
	name, ip := r.URL.Query().Get("pool"), net.ParseIP(r.URL.Query().Get("ip"))
	if ip == nil {
		return nil, "", errors.New("ip must be an IP address")
	}
	for _, p := range s.pools {
		if p.name != name {
			continue
		}
		if p.prom == nil {
			return nil, "", errNoMetrics
		}
		// Through mgmt the instance is the IP; a pool's own Prometheus
		// scrapes <ip>:9100.
		host := `instance=~"` + strings.ReplaceAll(regexp.QuoteMeta(ip.String()), `\`, `\\`) + `(:[0-9]+)?"`
		return p, matchers(p.selector(), host), nil
	}
	return nil, "", errors.New("no pool " + strconv.Quote(name))
}

// hostQuick is the host's "Quick CPU / Mem / Disk" values.
func (s *Server) hostQuick(w http.ResponseWriter, r *http.Request) {
	p, sel, err := s.hostOf(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		out  = map[string]float64{}
		errs []error
	)
	for name, q := range quickQueries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.prom.query(ctx, expand(q, sel, time.Minute))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else if len(res) > 0 {
				out[name] = res[0].Value
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// hostSeries is one panel of the host, its lines in the panel's order.
func (s *Server) hostSeries(w http.ResponseWriter, r *http.Request) {
	p, sel, err := s.hostOf(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	pn, ok := hostPanels[r.URL.Query().Get("panel")]
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown panel"))
		return
	}
	out, errs := drawPanel(r, pn, []*pool{p}, []string{sel}, false)
	if errs[0] != nil {
		writeError(w, http.StatusBadGateway, errs[0])
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// drawPanel queries a panel's lines from each pool, with that pool's
// matchers, and puts them on one grid; byVolume names each line's volume
// from its name label. A pool that fails has its error in errs and no lines.
func drawPanel(r *http.Request, pn panel, pools []*pool, sels []string, byVolume bool) (*Series, []error) {
	start, end, step := window(r)
	win := max(step, time.Minute)
	res := make([][][]series, len(pools))
	errs := make([]error, len(pools))
	each(pools, func(i int, p *pool) {
		if p.prom == nil {
			errs[i] = errNoMetrics
			return
		}
		res[i] = make([][]series, len(pn.lines))
		lineErrs := make([]error, len(pn.lines))
		var wg sync.WaitGroup
		for j, l := range pn.lines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res[i][j], lineErrs[j] = p.prom.queryRange(r.Context(), expand(l.expr, sels[i], win), start, end, step)
			}()
		}
		wg.Wait()
		errs[i] = errors.Join(lineErrs...)
	})

	type labeled struct {
		pool int
		line Line
		sr   series
	}
	var lines []labeled
	perItem := false
	for i, p := range pools {
		if errs[i] != nil {
			continue
		}
		for j, l := range pn.lines {
			perItem = perItem || strings.Contains(l.legend, "{{")
			for _, sr := range res[i][j] {
				line := Line{Pool: p.name, Label: legend(l.legend, sr.Labels)}
				if byVolume {
					line.Volume = sr.Labels["name"]
				}
				lines = append(lines, labeled{i, line, sr})
			}
		}
	}
	// A per-device or per-volume panel is ordered by its label, so a
	// device's read and write sit together; others keep the panel's order.
	if perItem {
		sort.SliceStable(lines, func(a, b int) bool {
			if lines[a].pool != lines[b].pool {
				return lines[a].pool < lines[b].pool
			}
			return naturalLess(lines[a].line.Label, lines[b].line.Label)
		})
	}
	out := grid(start, end, step)
	out.Unit = pn.unit
	for _, l := range lines {
		out.addLine(l.line, l.sr)
	}
	return out, errs
}

// naturalLess orders runs of digits by value: nvme2n1 before nvme10n1.
func naturalLess(a, b string) bool {
	for a != "" && b != "" {
		da, db := digitsPrefix(a), digitsPrefix(b)
		if da != "" && db != "" {
			na, _ := strconv.Atoi(da)
			nb, _ := strconv.Atoi(db)
			if na != nb {
				return na < nb
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitsPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

var legendLabel = regexp.MustCompile(`\{\{(\w+)\}\}`)

// legend fills a line's {{label}}s from its series' labels.
func legend(tmpl string, labels map[string]string) string {
	return legendLabel.ReplaceAllStringFunc(tmpl, func(m string) string { return labels[m[2:len(m)-2]] })
}
