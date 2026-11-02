package dash

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGateway accepts admin/secret for cluster main.
type fakeGateway struct{}

func (fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	_ = json.NewDecoder(r.Body).Decode(&in)
	if r.URL.Path != "/api/v1/auth" || in["username"] != "admin" || in["password"] != "secret" || in["cluster"] != "main" || in["ns"] != "main" {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":"wrong login or password"}`)
		return
	}
	fmt.Fprint(w, `{"access_token":"tok"}`)
}

// fakeProm is a management plane's Prometheus: two pools' series, each
// labelled with its pool. A query's pool="x" matcher picks one pool's.
type fakeProm struct {
	mu      sync.Mutex
	queries []string
}

var poolMatcher = regexp.MustCompile(`pool="([^"]+)"`)

// The pools' nodes: pool1 has a and b, pool2 has c. Node b is DOWN in its
// cluster, its core is not scraped, and its cache is failing.
var fakeNodes = []struct{ pool, host, ip, data, uuid, status string }{
	{"pool1", "node-a", "10.0.1.10", "10.0.2.10", "u1", "UP"},
	{"pool1", "node-b", "10.0.1.11", "10.0.2.11", "u2", "DOWN"},
	{"pool2", "node-c", "10.1.1.10", "10.1.2.10", "u3", "UP"},
}

func (p *fakeProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	p.mu.Lock()
	p.queries = append(p.queries, q)
	p.mu.Unlock()
	want := ""
	if m := poolMatcher.FindStringSubmatch(q); m != nil {
		want = m[1]
	}
	var rows []string
	add := func(pool, metric, value string) {
		if want == "" || want == pool {
			rows = append(rows, fmt.Sprintf(`{"metric":{%s,"pool":%q},"value":[1,%q]}`, metric, pool, value))
		}
	}
	if r.URL.Path == "/api/v1/query_range" {
		start := r.URL.Query().Get("start")
		for _, n := range fakeNodes {
			if want == "" || want == n.pool {
				// Volume charts are summed by name: one volume per node.
				key := fmt.Sprintf(`"ip":%q`, n.ip)
				if strings.Contains(q, "by(name)") {
					key = fmt.Sprintf(`"name":"vol-%s"`, n.host)
				} else if strings.Contains(q, "node_") {
					key = fmt.Sprintf(`"instance":%q,"node":%q`, n.ip, n.ip) // as mgmt federates node_exporter
				}
				rows = append(rows, fmt.Sprintf(`{"metric":{%s,"pool":%q},"values":[[%s,"5"]]}`, key, n.pool, start))
			}
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[%s]}}`, strings.Join(rows, ","))
		return
	}
	for _, n := range fakeNodes {
		bad := n.host == "node-b"
		switch {
		case strings.HasPrefix(q, "core_cluster_nodes"):
			add(n.pool, fmt.Sprintf(`"hostname":%q,"ip":%q,"uuid":%q,"status":%q`, n.host, n.ip, n.uuid, n.status), "1")
		case strings.HasPrefix(q, "up"):
			add(n.pool, fmt.Sprintf(`"job":"core","instance":"%s:8082"`, n.ip), map[bool]string{true: "0", false: "1"}[bad])
			add(n.pool, fmt.Sprintf(`"job":"cache","instance":"%s:8082"`, n.ip), "1")
		case strings.HasPrefix(q, "topk(1, cache_node_status"):
			add(n.pool, fmt.Sprintf(`"node":%q,"ip":%q,"errors":%q,"attached_mounts":"1","node_disks_count":"10","rw_cache_size_in_mib":"100","r_cache_size_in_mib":"300"`,
				n.uuid, n.data, map[bool]string{true: "0", false: "1"}[bad]), "1")
		case strings.HasPrefix(q, "topk(1, cache_raid_member_state"):
			for _, dev := range []string{"/dev/nvme10n1", "/dev/nvme2n1"} {
				state, failed := "active sync", "1"
				if bad && dev == "/dev/nvme2n1" {
					state, failed = "faulty", "0"
				}
				add(n.pool, fmt.Sprintf(`"node":%q,"ip":%q,"raid":"/dev/md/mgx-%s","raid_device":%q,"device_state":%q,"raid_level":"raid0","raid_state":"clean ","raid_state_status":"1","failed_devices":%q,"total_devices":"2"`,
					n.uuid, n.data, n.uuid, dev, state, failed), "1")
			}
		case strings.HasPrefix(q, "topk(1, storage_volume_status"):
			// One volume per node, served by it; node b's is not ready.
			st, v := "READY", "1"
			if bad {
				st, v = "ERROR", "0"
			}
			add(n.pool, fmt.Sprintf(`"name":"vol-%s","status":%q,"size":"10240","cache_rw_cache_size":"1024","cache_r_cache_size":"2048","storage_bucket":"bkt","sc_node":%q,"ip":%q`,
				n.host, st, n.uuid, n.ip), v)
		case strings.Contains(q, "node_uname_info"):
			add(n.pool, fmt.Sprintf(`"instance":%q,"node":%q,"nodename":"ip-%s","release":"6.8"`, n.ip, n.ip, n.host), "1")
		case strings.Contains(q, "node_"):
			add(n.pool, fmt.Sprintf(`"instance":%q,"node":%q`, n.ip, n.ip), "42")
		case strings.HasPrefix(q, "topk(1, cache_nvme_mounts"):
			for _, kind := range []string{"r", "rw"} {
				add(n.pool, fmt.Sprintf(`"node":%q,"mountpoint":"/mnt/s3cache/%s/%s","device":"/dev/md127","opts":"rw","nvme_mounts_status":%q`,
					n.uuid, kind, n.uuid, map[bool]string{true: "0", false: "1"}[bad]), "1")
			}
		}
	}
	fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(rows, ","))
}

// newTestServer serves pool1 and pool2 from one management-plane Prometheus,
// and pool3 without metrics.
func newTestServer(t *testing.T) (*httptest.Server, *fakeProm) {
	gws := httptest.NewServer(fakeGateway{})
	t.Cleanup(gws.Close)
	prom := &fakeProm{}
	ps := httptest.NewServer(prom)
	t.Cleanup(ps.Close)
	u, _ := url.Parse(gws.URL)
	s := New(Config{Cluster: "main", Gateway: u.Host, Pools: []Pool{
		{Name: "pool1", Prometheus: ps.URL, Label: "pool1"},
		{Name: "pool2", Prometheus: ps.URL, Label: "pool2"},
		{Name: "pool3"},
	}})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, prom
}

type client struct {
	t      *testing.T
	base   string
	cookie string
}

func (c *client) do(method, path, body string) (int, map[string]any) {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.base+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	for _, ck := range resp.Cookies() {
		if ck.Name == cookieName {
			c.cookie = ck.Name + "=" + ck.Value
		}
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func login(t *testing.T, srv *httptest.Server) *client {
	t.Helper()
	c := &client{t: t, base: srv.URL}
	if code, out := c.do("POST", "/api/login", `{"username":"admin","password":"secret"}`); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	return c
}

func TestLogin(t *testing.T) {
	srv, _ := newTestServer(t)
	c := &client{t: t, base: srv.URL}
	if code, _ := c.do("GET", "/api/overview", ""); code != 401 {
		t.Fatalf("overview before login: %d, want 401", code)
	}
	if code, out := c.do("POST", "/api/login", `{"username":"admin","password":"nope"}`); code != 401 || !strings.Contains(out["error"].(string), "wrong login") {
		t.Fatalf("bad password: %d %v", code, out)
	}
	c = login(t, srv)
	if code, _ := c.do("GET", "/api/overview", ""); code != 200 {
		t.Fatalf("after login: %d", code)
	}
	c.do("POST", "/api/logout", "")
	if code, _ := c.do("GET", "/api/overview", ""); code != 401 {
		t.Errorf("after logout: %d, want 401", code)
	}
}

func TestOverview(t *testing.T) {
	srv, _ := newTestServer(t)
	c := login(t, srv)

	_, out := c.do("GET", "/api/overview?pool=pool1", "")
	nodes := out["nodes"].([]any)
	if len(nodes) != 2 || out["warnings"] != nil {
		t.Fatalf("pool1: %v", out)
	}
	a, b := nodes[0].(map[string]any), nodes[1].(map[string]any)
	if a["hostname"] != "node-a" || a["uuid"] != "u1" || a["status"] != "UP" || a["pool"] != "pool1" || a["kernel"] != "6.8" {
		t.Errorf("node a: %v", a)
	}
	if s := b["services"].(map[string]any); b["status"] != "DOWN" || s["core"] != false || s["cache"] != true {
		t.Errorf("node b: %v", b)
	}
	// node_exporter readings join the node by IP.
	vals := a["values"].(map[string]any)
	for k := range hostQueries {
		if k != "uname" && vals[k] != 42.0 {
			t.Errorf("node a value %s = %v", k, vals[k])
		}
	}

	// Every pool: pool1's and pool2's nodes, and a warning for pool3.
	_, out = c.do("GET", "/api/overview?pool=all", "")
	if n := len(out["nodes"].([]any)); n != 3 {
		t.Errorf("all pools: %d nodes, want 3", n)
	}
	if w, _ := out["warnings"].([]any); len(w) != 1 || !strings.HasPrefix(w[0].(string), "pool3: no metrics") {
		t.Errorf("all pools warnings: %v", w)
	}
	// Several pools: pool2 and pool3, in the configured order.
	_, out = c.do("GET", "/api/overview?pool=pool3,pool2", "")
	if n := out["nodes"].([]any); len(n) != 1 || n[0].(map[string]any)["pool"] != "pool2" {
		t.Errorf("pool2,pool3: %v", out)
	}
	if code, _ := c.do("GET", "/api/overview?pool=pool1,nope", ""); code != 400 {
		t.Errorf("unknown pool: %d, want 400", code)
	}
}

func TestSeries(t *testing.T) {
	srv, prom := newTestServer(t)
	c := login(t, srv)

	if code, _ := c.do("GET", "/api/series?metric=up", ""); code != 400 {
		t.Errorf("unknown metric: %d, want 400", code)
	}
	code, out := c.do("GET", "/api/series?metric=host_cpu&range=900&pool=pool1", "")
	if code != 200 {
		t.Fatalf("series: %d %v", code, out)
	}
	if ts := out["t"].([]any); len(ts) != 61 { // 900s at the 15s floor, both ends included
		t.Errorf("points: %d, want 61", len(ts))
	}
	lines := out["series"].([]any)
	first := lines[0].(map[string]any)
	if len(lines) != 2 || first["ip"] != "10.0.1.10" || first["pool"] != "pool1" || first["v"].([]any)[0] != 5.0 || first["v"].([]any)[1] != nil {
		t.Errorf("aligned series: %v", lines)
	}
	_, out = c.do("GET", "/api/series?metric=host_ram&pool=all", "")
	if n := len(out["series"].([]any)); n != 3 {
		t.Errorf("all pools: %d lines, want 3", n)
	}
	for _, q := range prom.queries {
		if !poolMatcher.MatchString(q) {
			t.Errorf("query without a pool matcher: %s", q)
		}
	}
}

func TestCache(t *testing.T) {
	srv, _ := newTestServer(t)
	c := login(t, srv)

	code, out := c.do("GET", "/api/cache?pool=pool1", "")
	if code != 200 || out["warnings"] != nil {
		t.Fatalf("cache: %d %v", code, out)
	}
	sum := out["summary"].(map[string]any)
	want := map[string]float64{"nodes_up": 1, "nodes_down": 1, "raids_down": 1, "mounts_down": 1, "disks": 20, "rw_mib": 2000, "r_mib": 6000}
	for k, v := range want {
		if sum[k] != v {
			t.Errorf("summary %s = %v, want %v", k, sum[k], v)
		}
	}
	nodes := out["nodes"].([]any)
	if n := nodes[0].(map[string]any); n["hostname"] != "node-a" || n["pool"] != "pool1" || n["errors"] != true {
		t.Errorf("node a (hostname from core_cluster_nodes): %v", n)
	}
	raids := out["raids"].([]any)
	r := raids[0].(map[string]any)
	if r["ok"] != true || r["members"].([]any)[0].(map[string]any)["device"] != "/dev/nvme2n1" {
		t.Errorf("raid a (members in device order): %v", r)
	}
	if r := raids[1].(map[string]any); r["ok"] != false || r["no_failed"] != false {
		t.Errorf("raid b should be down: %v", r)
	}
	if m := out["mounts"].([]any)[0].(map[string]any); m["kind"] != "rw" || m["ok"] != true {
		t.Errorf("first mount (rw before r): %v", out["mounts"])
	}

	_, out = c.do("GET", "/api/cache?pool=all", "")
	if sum := out["summary"].(map[string]any); sum["nodes_up"] != 2.0 || sum["disks"] != 30.0 {
		t.Errorf("all pools summary: %v", sum)
	}
}

func TestStorage(t *testing.T) {
	srv, prom := newTestServer(t)
	c := login(t, srv)

	code, out := c.do("GET", "/api/storage?pool=pool1", "")
	if code != 200 || out["warnings"] != nil {
		t.Fatalf("storage: %d %v", code, out)
	}
	sum := out["summary"].(map[string]any)
	want := map[string]float64{"volumes": 2, "not_ready": 1, "size_mib": 20480, "rw_mib": 2048, "r_mib": 4096}
	for k, v := range want {
		if sum[k] != v {
			t.Errorf("summary %s = %v, want %v", k, sum[k], v)
		}
	}
	vols := out["volumes"].([]any)
	if v := vols[0].(map[string]any); v["name"] != "vol-node-a" || v["ready"] != true || v["hostname"] != "node-a" || v["pool"] != "pool1" {
		t.Errorf("volume a (hostname from core_cluster_nodes): %v", v)
	}
	if v := vols[1].(map[string]any); v["ready"] != false || v["status"] != "ERROR" {
		t.Errorf("volume b should not be ready: %v", v)
	}

	_, out = c.do("GET", "/api/storage?pool=all", "")
	if w := out["warnings"].([]any); len(w) != 1 || out["summary"].(map[string]any)["volumes"] != 3.0 {
		t.Errorf("all pools: %v (pool3 has no metrics)", out)
	}

	_, out = c.do("GET", "/api/series?metric=read_mbps&pool=pool2", "")
	lines := out["series"].([]any)
	if l := lines[0].(map[string]any); len(lines) != 1 || l["volume"] != "vol-node-c" || l["ip"] != nil {
		t.Errorf("volume series: %v", lines)
	}
	last := prom.queries[len(prom.queries)-1]
	if !strings.Contains(last, `sum by(name) (spdk_volume_bytes_read{pool="pool2"})`) {
		t.Errorf("volume query: %s", last)
	}
}

func TestHostDetail(t *testing.T) {
	srv, prom := newTestServer(t)
	c := login(t, srv)

	code, out := c.do("GET", "/api/host/quick?pool=pool1&ip=10.0.1.10", "")
	if code != 200 || out["cpu_busy"] != 42.0 || out["uptime"] != 42.0 {
		t.Fatalf("quick: %d %v", code, out)
	}
	host := `instance=~"10\\.0\\.1\\.10(:[0-9]+)?"`
	for _, q := range prom.queries {
		if strings.Contains(q, "{sel}") || strings.Contains(q, "{win}") || !strings.Contains(q, host) || !poolMatcher.MatchString(q) {
			t.Errorf("quick query: %s", q)
		}
	}

	code, out = c.do("GET", "/api/host/series?pool=pool1&ip=10.0.1.10&panel=psi&range=3600", "")
	if code != 200 || out["unit"] != "percent" {
		t.Fatalf("panel: %d %v", code, out)
	}
	if l := out["series"].([]any)[0].(map[string]any); l["label"] != "CPU some" {
		t.Errorf("first line (in the panel's order): %v", l)
	}
	code, out = c.do("GET", "/api/host/series?pool=pool1&ip=10.0.1.10&panel=disk_iops", "")
	if l := out["series"].([]any)[0].(map[string]any); code != 200 || l["label"] != " read" {
		t.Errorf("legend with a missing label: %d %v", code, l)
	}

	for _, bad := range []string{
		"/api/host/quick?pool=pool1&ip=10.0.1.10%22%7D",
		"/api/host/quick?pool=nope&ip=10.0.1.10",
		"/api/host/quick?pool=pool3&ip=10.0.1.10",
		"/api/host/series?pool=pool1&ip=10.0.1.10&panel=up",
	} {
		if code, _ := c.do("GET", bad, ""); code != 400 {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
}

func TestHostPanels(t *testing.T) {
	// Every query of every panel names its pool and host, and its rate window.
	for name, p := range hostPanels {
		if p.unit == "" || len(p.lines) == 0 {
			t.Errorf("%s: no unit or lines", name)
		}
		for _, l := range p.lines {
			q := expand(l.expr, `pool="p"`, time.Minute)
			if strings.Contains(q, "{sel}") || strings.Contains(q, "{win}") {
				t.Errorf("%s: unexpanded %s", name, q)
			}
			if strings.Count(q, `pool="p"`) != strings.Count(q, "node_") {
				t.Errorf("%s: a metric without the pool matcher: %s", name, q)
			}
		}
	}
}

func TestNaturalLess(t *testing.T) {
	in := []string{"nvme10n1 read", "nvme1n1 read", "md127 read", "nvme2n1 read", "nvme1n1"}
	sort.Slice(in, func(i, j int) bool { return naturalLess(in[i], in[j]) })
	if want := "md127 read,nvme1n1,nvme1n1 read,nvme2n1 read,nvme10n1 read"; strings.Join(in, ",") != want {
		t.Errorf("got %v, want %s", in, want)
	}
}

func TestExpand(t *testing.T) {
	for _, c := range []struct{ in, sel, want string }{
		{`m{{sel}}`, ``, `m{}`},
		{`m{{sel}}`, `pool="a"`, `m{pool="a"}`},
		{`m{x="1"{sel}}[{win}]`, ``, `m{x="1"}[1m0s]`},
		{`m{x="1"{sel}}[{win}]`, `pool="a"`, `m{x="1",pool="a"}[1m0s]`},
	} {
		if got := expand(c.in, c.sel, time.Minute); got != c.want {
			t.Errorf("expand(%q, %q) = %q, want %q", c.in, c.sel, got, c.want)
		}
	}
}

func TestLocalOnly(t *testing.T) {
	srv, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/info", nil)
	req.Host = "evil.example:8080"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Errorf("foreign Host: %d, want 403", resp.StatusCode)
	}
}

func TestScrapedHost(t *testing.T) {
	for in, want := range map[string]string{
		"instance=10.0.0.1:8082":                "10.0.0.1",
		"instance=10.0.0.2":                     "10.0.0.2",
		"instance=localhost:8082 node=10.0.0.3": "10.0.0.3",
		"instance=localhost:8082":               "",
	} {
		l := map[string]string{}
		for _, kv := range strings.Fields(in) {
			k, v, _ := strings.Cut(kv, "=")
			l[k] = v
		}
		if got := scrapedHost(l); got != want {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
}
