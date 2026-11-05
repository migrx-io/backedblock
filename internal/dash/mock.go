package dash

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MockSize is the shape of a mock cluster: pools of nodes, each node serving
// volumes and holding snapshots of them.
type MockSize struct{ Pools, Nodes, Volumes, Snapshots int }

// StartMock serves a made-up cluster of size on loopback: a cluster API that
// takes any password and a management-plane Prometheus with every pool's
// series. The returned Config points the dashboard at them, to see how it
// looks at a size no dev cluster has. Every name and value comes from its
// pool, node and index, so restarts show the same cluster; a few nodes,
// disks, volumes and snapshots are failing, for the status views.
func StartMock(size MockSize) (Config, func(), error) {
	m := &mock{size: size}
	gw, err := serveLoopback(http.HandlerFunc(mockGateway))
	if err != nil {
		return Config{}, nil, err
	}
	prom, err := serveLoopback(m)
	if err != nil {
		gw.Close()
		return Config{}, nil, err
	}
	cfg := Config{Cluster: "mock", Gateway: gw.Addr, LoginHint: "any password"}
	for p := range size.Pools {
		name := m.pool(p)
		cfg.Pools = append(cfg.Pools, Pool{Name: name, Prometheus: "http://" + prom.Addr, Label: name})
	}
	return cfg, func() { gw.Close(); prom.Close() }, nil
}

func serveLoopback(h http.Handler) (*http.Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Addr: ln.Addr().String(), Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return srv, nil
}

// mockGateway is the cluster API's login: any password for admin.
func mockGateway(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	if r.URL.Path != "/api/v1/auth" || in["username"] != "admin" || in["password"] == "" {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"wrong login or password"}`)
		return
	}
	fmt.Fprint(w, `{"access_token":"mock"}`)
}

type mock struct{ size MockSize }

// hash is a stable number for a name and indices: what varies between
// nodes, volumes and snapshots.
func hash(parts ...any) uint64 {
	h := fnv.New64a()
	fmt.Fprint(h, parts...)
	return h.Sum64()
}

func (m *mock) pool(p int) string { return fmt.Sprintf("pool%d", p+1) }

type mockNode struct {
	pool, ip, data, uuid, host string
	p, n                       int
	down                       bool // DOWN in its cluster, its core not scraped
	badCache                   bool // a failed RAID member, its mounts down
}

func (m *mock) node(p, n int) mockNode {
	ip := fmt.Sprintf("10.%d.%d.%d", 100+p/250, p%250, 10+n)
	h := hash("node", p, n)
	return mockNode{
		pool: m.pool(p), ip: ip, data: fmt.Sprintf("10.%d.%d.%d", 200+p/250, p%250, 10+n),
		uuid: fmt.Sprintf("%08x-%04x-5%03x-a%03x-%012x", uint32(h), uint16(h>>32), h>>48&0xfff, h>>36&0xfff, h&0xffffffffffff),
		host: "ip-" + strings.ReplaceAll(ip, ".", "-") + ".ec2.internal",
		p:    p, n: n, down: h%53 == 7, badCache: h%61 == 3,
	}
}

// nodes are the nodes of the pools a query names (pool="x"), or all.
func (m *mock) nodes(pools map[int]bool) []mockNode {
	var out []mockNode
	for p := range m.size.Pools {
		if pools != nil && !pools[p] {
			continue
		}
		for n := range m.size.Nodes {
			out = append(out, m.node(p, n))
		}
	}
	return out
}

func volumeName(p, n, v int) string { return fmt.Sprintf("vol-%016x", hash("vol", p, n, v)) }

var (
	mockPool  = regexp.MustCompile(`pool="pool(\d+)"`)
	mockHost  = regexp.MustCompile(`instance=~"([^"(]+)`)
	mockNames = regexp.MustCompile(`name=~"([^"]+)"`)
	mockIPs   = regexp.MustCompile(`ip=~"([^"]+)"`)
)

// scope is what a query asks for: its pools and, for a host panel, the node.
func (m *mock) scope(q string) (map[int]bool, string) {
	var pools map[int]bool
	for _, s := range mockPool.FindAllStringSubmatch(q, -1) {
		if pools == nil {
			pools = map[int]bool{}
		}
		p, _ := strconv.Atoi(s[1])
		pools[p-1] = true
	}
	host := ""
	if s := mockHost.FindStringSubmatch(q); s != nil {
		host = strings.ReplaceAll(s[1], `\`, "")
	}
	return pools, host
}

// mockRow is one sample of an instant query.
type mockRow struct {
	labels map[string]string
	value  float64
}

func (m *mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	q := r.Form.Get("query")
	pools, host := m.scope(q)
	var rows []mockRow
	add := func(n mockNode, value float64, kv ...string) {
		l := map[string]string{"pool": n.pool}
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		rows = append(rows, mockRow{l, value})
	}
	nodes := m.nodes(pools)
	if host != "" {
		var only []mockNode
		for _, n := range nodes {
			if n.ip == host {
				only = append(only, n)
			}
		}
		nodes = only
	}

	if r.URL.Path == "/api/v1/query_range" {
		m.ranged(w, r, q, nodes)
		return
	}
	switch {
	case strings.HasPrefix(q, "core_cluster_nodes"):
		for _, n := range nodes {
			add(n, 1, "hostname", n.host, "ip", n.ip, "uuid", n.uuid, "status", map[bool]string{true: "DOWN", false: "UP"}[n.down])
		}
	case strings.HasPrefix(q, "up{"):
		for _, n := range nodes {
			for _, job := range []string{"core", "cache", "storage"} {
				v := 1.0
				if n.down && job == "core" {
					v = 0
				}
				add(n, v, "job", job, "app", job, "instance", n.ip, "node", n.ip)
			}
		}
	case strings.Contains(q, "node_uname_info"):
		for _, n := range nodes {
			add(n, 1, "instance", n.ip, "node", n.ip, "nodename", n.host, "release", "6.8.0-1066-aws")
		}
	case strings.HasPrefix(q, "topk(1, cache_node_status"):
		for _, n := range nodes {
			add(n, 1, "node", n.uuid, "ip", n.data, "errors", map[bool]string{true: "0", false: "1"}[n.badCache],
				"attached_mounts", "1", "node_disks_count", "10", "rw_cache_size_in_mib", "131321", "r_cache_size_in_mib", "437737")
		}
	case strings.HasPrefix(q, "topk(1, cache_raid_member_state"):
		for _, n := range nodes {
			for d := range 10 {
				state, failed := "active sync", "1"
				if n.badCache && d == 3 {
					state, failed = "faulty", "0"
				}
				add(n, 1, "node", n.uuid, "ip", n.data, "raid", "/dev/md127", "raid_device", fmt.Sprintf("/dev/nvme%dn1", d+1),
					"device_state", state, "raid_level", "raid0", "raid_state", "clean ", "raid_state_status", "1",
					"failed_devices", failed, "total_devices", "10")
			}
		}
	case strings.HasPrefix(q, "topk(1, cache_nvme_mounts"):
		for _, n := range nodes {
			for _, kind := range []string{"r", "rw"} {
				add(n, 1, "node", n.uuid, "mountpoint", "/mnt/s3cache/"+kind+"/"+n.uuid, "device", "/dev/md127",
					"opts", "rw,noatime,nodiratime,stripe=1280", "nvme_mounts_status", map[bool]string{true: "0", false: "1"}[n.badCache])
			}
		}
	case strings.HasPrefix(q, "topk(1, storage_volume_status"):
		for _, n := range nodes {
			for v := range m.size.Volumes {
				h := hash("vol", n.p, n.n, v)
				st, ready := "READY", 1.0
				switch {
				case h%97 == 5:
					st, ready = "ERROR", 0
				case h%89 == 11:
					st, ready = "CREATING", 0
				}
				size := []int{102400, 512000, 1048576, 5242880}[h%4]
				add(n, ready, "name", volumeName(n.p, n.n, v), "status", st, "size", strconv.Itoa(size),
					"cache_rw_cache_size", strconv.Itoa(size*3/1000+1024), "cache_r_cache_size", strconv.Itoa(size/100+1024),
					"storage_bucket", fmt.Sprintf("mock-%s-data", n.pool), "sc_node", n.uuid, "ip", n.ip,
					"qos_rw_mbytes_per_sec", []string{"125", "250", "500", "None"}[h>>8%4], "qos_rw_ios_per_sec", []string{"3000", "8000", "16000"}[h>>12%3],
					"qos_r_mbytes_per_sec", "None", "qos_w_mbytes_per_sec", "None",
					"extra_params", "--nbd-flag=--threads=32 --nbd-param=cache-flush-threads=10 --nbd-param=cache-flush-interval=300 --nbd-param=cache-high-threshold=95 --nbd-param=cache-low-threshold=85")
			}
		}
	case strings.HasPrefix(q, "topk(1, snapshot_status"):
		for _, n := range nodes {
			for s := range m.size.Snapshots {
				st, ready := m.snapshot(n, s)
				v := 0
				if m.size.Volumes > 0 {
					v = s % m.size.Volumes
				}
				add(n, ready, "name", fmt.Sprintf("snap-%016x", hash("snap", n.p, n.n, s)), "status", st, "kind", "snapshot",
					"storage_pool", n.pool, "sc_node", n.uuid, "volume", volumeName(n.p, n.n, v), "config", n.pool,
					"incremental", map[bool]string{true: "yes", false: "no"}[s > 0], "mode", "copy")
			}
		}
	case strings.HasPrefix(q, `{__name__=~"snapshot_`):
		for _, n := range nodes {
			for s := range m.size.Snapshots {
				h := hash("snap", n.p, n.n, s)
				name := fmt.Sprintf("snap-%016x", h)
				bytes := float64(h%5000+1) * 1048576
				st, _ := m.snapshot(n, s)
				for metric, v := range map[string]float64{
					"snapshot_size": float64([]int{102400, 512000, 1048576, 5242880}[h%4]), "snapshot_bytes": bytes,
					"snapshot_server_side_copy_bytes": bytes * float64(80+h%21) / 100, "snapshot_errors": float64(map[bool]int{true: 3}[st == "FAILED"]),
					"snapshot_fatal_error": float64(map[bool]int{true: 1}[st == "FAILED"]), "snapshot_retry_error": float64(h % 2 * map[bool]uint64{true: 1}[st == "FAILED"]),
					"snapshot_elapsed_time": float64(h%600 + 5), "snapshot_transfer_time": float64(h%400 + 3),
					"snapshot_total_transfers": float64(h%3000 + 10), "snapshot_speed": float64(h%200) * 1048576,
				} {
					add(n, v, "__name__", metric, "name", name, "storage_pool", n.pool)
				}
			}
		}
	default:
		// A Hosts "Quick" value: one number for the node.
		if host != "" && len(nodes) > 0 {
			add(nodes[0], m.quick(q, nodes[0]))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	b := []byte(`{"status":"success","data":{"resultType":"vector","result":[`)
	for i, s := range rows {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendMetric(b, s.labels)
		b = append(b, `,"value":[`...)
		b = strconv.AppendInt(b, time.Now().Unix(), 10)
		b = append(b, `,"`...)
		b = strconv.AppendFloat(b, s.value, 'g', -1, 64)
		b = append(b, `"]}`...)
	}
	_, _ = w.Write(append(b, "]}}"...))
}

// snapshot is a snapshot's status: a few failed, a few still running.
func (m *mock) snapshot(n mockNode, s int) (string, float64) {
	h := hash("snap", n.p, n.n, s)
	switch {
	case h%41 == 9:
		return "FAILED", 0
	case h%37 == 2:
		return "RUNNING", 0
	}
	return "READY", 1
}

// quick is a Hosts "Quick" value, by what its query reads.
func (m *mock) quick(q string, n mockNode) float64 {
	h := float64(hash("quick", n.p, n.n) % 1000)
	switch {
	case strings.Contains(q, "node_pressure"):
		return h / 400
	case strings.Contains(q, `mode="idle"`):
		return 5 + h/15
	case strings.Contains(q, "node_load1"):
		return 3 + h/12
	case strings.Contains(q, "MemAvailable"):
		return 20 + h/20
	case strings.Contains(q, "SwapTotal_bytes{") && strings.Contains(q, "SwapFree"):
		return 0
	case strings.Contains(q, "node_filesystem_size_bytes") && strings.HasPrefix(q, "100 *"):
		return 10 + h/40
	case strings.Contains(q, "count(count("):
		return 16
	case strings.Contains(q, "MemTotal"):
		return 32 << 30
	case strings.Contains(q, "SwapTotal"):
		return 0
	case strings.Contains(q, "node_filesystem_size_bytes"):
		return 30 << 30
	case strings.Contains(q, "node_time_seconds"):
		return 3600 + h*400
	}
	return 0
}

// ranged answers a chart: one line per node, volume, device or filesystem
// the query is about, moving through the range.
func (m *mock) ranged(w http.ResponseWriter, r *http.Request, q string, nodes []mockNode) {
	start, _ := strconv.ParseInt(r.Form.Get("start"), 10, 64)
	end, _ := strconv.ParseInt(r.Form.Get("end"), 10, 64)
	step, err := time.ParseDuration(r.Form.Get("step"))
	if err != nil {
		s, _ := strconv.ParseFloat(r.Form.Get("step"), 64)
		step = time.Duration(s * float64(time.Second))
	}
	st := int64(max(step/time.Second, 1))

	type line struct {
		labels map[string]string
		seed   uint64
	}
	var lines []line
	each := func(n mockNode, kv ...string) {
		l := map[string]string{"pool": n.pool}
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		lines = append(lines, line{l, hash(q, n.p, n.n, kv)})
	}
	switch {
	case strings.Contains(q, "core_cluster_system"):
		// The nodes it names (ip=~"a|b").
		var want map[string]bool
		if s := mockIPs.FindStringSubmatch(q); s != nil {
			want = map[string]bool{}
			for _, ip := range strings.Split(strings.ReplaceAll(s[1], `\`, ""), "|") {
				want[ip] = true
			}
		}
		for _, n := range nodes {
			if want == nil || want[n.ip] {
				each(n, "ip", n.ip, "node", n.ip)
			}
		}
	case strings.Contains(q, "node_disk"):
		for _, n := range nodes {
			devs := []string{"md127"}
			for d := range 11 {
				devs = append(devs, fmt.Sprintf("nvme%dn1", d))
			}
			if strings.Contains(q, "nbd") {
				for d := range 16 {
					devs = append(devs, fmt.Sprintf("nbd%d", d))
				}
			}
			for _, d := range devs {
				each(n, "instance", n.ip, "node", n.ip, "device", d)
			}
		}
	case strings.Contains(q, "node_network"):
		for _, n := range nodes {
			each(n, "instance", n.ip, "node", n.ip, "device", "ens5")
		}
	case strings.Contains(q, "node_filesystem"):
		for _, n := range nodes {
			for _, mp := range []string{"/", "/boot/efi", "/run", "/mnt/s3cache/r/" + n.uuid, "/mnt/s3cache/rw/" + n.uuid} {
				each(n, "instance", n.ip, "node", n.ip, "mountpoint", mp, "device", "/dev/nvme0n1p1")
			}
		}
	case strings.Contains(q, "node_"):
		for _, n := range nodes {
			each(n, "instance", n.ip, "node", n.ip)
		}
	default:
		// A Volumes panel: a line per volume it names (name=~"a|b").
		var want map[string]bool
		if s := mockNames.FindStringSubmatch(q); s != nil {
			want = map[string]bool{}
			for _, name := range strings.Split(strings.ReplaceAll(s[1], `\`, ""), "|") {
				want[name] = true
			}
		}
		for _, n := range nodes {
			for v := range m.size.Volumes {
				if name := volumeName(n.p, n.n, v); want == nil || want[name] {
					each(n, "name", name, "sc_node", n.uuid)
				}
			}
		}
	}

	scale := mockScale(q)
	w.Header().Set("Content-Type", "application/json")
	b := []byte(`{"status":"success","data":{"resultType":"matrix","result":[`)
	for i, l := range lines {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendMetric(b, l.labels)
		b = append(b, `,"values":[`...)
		// A third of the volumes are idle; the rest move and now and then
		// spike.
		idle := !strings.Contains(q, "node_") && !strings.Contains(q, "core_") && l.seed%3 == 0
		phase := float64(l.seed%628) / 100
		level := 0.3 + float64(l.seed%70)/100
		first := true
		for t := start; t <= end; t += st {
			v := 0.0
			if !idle {
				v = scale * level * (0.6 + 0.4*math.Sin(float64(t)/900+phase))
				if hash(l.seed, t/st)%97 == 0 {
					v *= 2.5
				}
				if scale <= 100 && strings.HasPrefix(q, "100 *") {
					v = math.Min(v, 100)
				}
			}
			if !first {
				b = append(b, ',')
			}
			first = false
			b = append(b, '[')
			b = strconv.AppendInt(b, t, 10)
			b = append(b, `,"`...)
			b = strconv.AppendFloat(b, v, 'f', 4, 64)
			b = append(b, `"]`...)
		}
		b = append(b, "]}"...)
	}
	_, _ = w.Write(append(b, "]}}"...))
}

// mockScale is a chart's typical size, by what its query reads.
func mockScale(q string) float64 {
	for _, s := range []struct {
		has   string
		scale float64
	}{
		{"core_cluster_system", 60},
		{"node_pressure", 3},
		{"100 *", 40},
		{"node_network", 2e9},
		{"bytes_total", 300e6},
		{"time_seconds_total", 0.004},
		{"io_now", 6},
		{"_total", 2000},
		{"node_memory_Mem", 30 << 30},
		{"node_memory", 4 << 30},
		{"node_filesystem", 200 << 30},
		{"node_", 50},
		{"hit_ratio", 1},
		{"latency", 400},
		{"spdk_volume_bytes", 400 << 20},
		{"_bytes", 2 << 30},
		{"cache_size", 4096},
		{"avg_", 50},
		{"error", 2},
	} {
		if strings.Contains(q, s.has) {
			return s.scale
		}
	}
	return 500
}

func appendMetric(b []byte, labels map[string]string) []byte {
	b = append(b, `{"metric":`...)
	raw, _ := json.Marshal(labels)
	return append(b, raw...)
}
