package dash

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
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
				rows = append(rows, fmt.Sprintf(`{"metric":{"ip":%q,"pool":%q},"values":[[%s,"5"]]}`, n.ip, n.pool, start))
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
		case strings.HasPrefix(q, "core_cluster_system_cpu"):
			add(n.pool, fmt.Sprintf(`"ip":%q`, n.ip), "12.5")
		case strings.HasPrefix(q, "core_cluster_system_ram"):
			add(n.pool, fmt.Sprintf(`"ip":%q`, n.ip), "40")
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
	if a["hostname"] != "node-a" || a["uuid"] != "u1" || a["status"] != "UP" || a["pool"] != "pool1" || a["cpu"] != 12.5 || a["ram"] != 40.0 {
		t.Errorf("node a: %v", a)
	}
	if s := b["services"].(map[string]any); b["status"] != "DOWN" || s["core"] != false || s["cache"] != true {
		t.Errorf("node b: %v", b)
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
	code, out := c.do("GET", "/api/series?metric=cpu&range=900&pool=pool1", "")
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
	_, out = c.do("GET", "/api/series?metric=ram&pool=all", "")
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
