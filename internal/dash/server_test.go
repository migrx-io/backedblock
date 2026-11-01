package dash

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/migrx-io/backedblock/internal/awsx"
)

// fakeGateway serves the parts of mgx-gateway-api the dashboard calls. Each
// login issues a new token; expire invalidates the current one.
type fakeGateway struct {
	logins  atomic.Int32
	valid   atomic.Value // string
	cluster string
}

func (g *fakeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && r.URL.Path == "/api/v1/auth":
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["username"] != "admin" || in["password"] != "secret" || in["cluster"] != g.cluster || in["ns"] != g.cluster {
			w.WriteHeader(500)
			fmt.Fprint(w, `{"error":"wrong login or password"}`)
			return
		}
		tok := fmt.Sprintf("tok-%d", g.logins.Add(1))
		g.valid.Store(tok)
		fmt.Fprintf(w, `{"access_token":%q}`, tok)
	case r.Header.Get("Authorization") != "JWT "+g.valid.Load().(string):
		w.WriteHeader(401)
		fmt.Fprint(w, `{"msg":"Token has expired"}`)
	case r.URL.Path == "/api/v1/cluster":
		fmt.Fprint(w, `[{"cluster":"main","vip_host":{"uuid":"u1","ip":"10.0.1.10","vip":"10.0.1.100"}}]`)
	case r.URL.Path == "/api/v1/cluster/main/nodes":
		fmt.Fprint(w, `[{"uuid":"u1","ip":"10.0.1.10","hostname":"node-a","status":"UP"},
			{"uuid":"u2","ip":"10.0.1.11","hostname":"node-b","status":"DOWN"}]`)
	default:
		w.WriteHeader(404)
	}
}

func (g *fakeGateway) expire() { g.valid.Store("expired") }

// fakeProm answers the dashboard's queries and records them.
type fakeProm struct{ queries []string }

func (p *fakeProm) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	p.queries = append(p.queries, q)
	vec := func(rows ...string) {
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, strings.Join(rows, ","))
	}
	switch {
	case r.URL.Path == "/api/v1/query" && strings.HasPrefix(q, "up"):
		vec(`{"metric":{"job":"core","instance":"10.0.1.10:8082"},"value":[1,"1"]}`,
			`{"metric":{"job":"cache","instance":"10.0.1.10:8082"},"value":[1,"0"]}`,
			// Through mgmt federation a node's own target is relabelled.
			`{"metric":{"job":"core","instance":"localhost:8082","node":"10.0.1.11"},"value":[1,"0"]}`)
	case r.URL.Path == "/api/v1/query" && strings.HasPrefix(q, "core_cluster_system_cpu"):
		vec(`{"metric":{"ip":"10.0.1.10","node":"u1"},"value":[1,"12.5"]}`)
	case r.URL.Path == "/api/v1/query":
		vec(`{"metric":{"ip":"10.0.1.10","node":"u1"},"value":[1,"40"]}`)
	case r.URL.Path == "/api/v1/query_range":
		start := r.URL.Query().Get("start")
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"ip":"10.0.1.11"},"values":[[%s,"5"]]},
			{"metric":{"ip":"10.0.1.10"},"values":[[%s,"7"]]}]}}`, start, start)
	}
}

func newTestServer(t *testing.T, poolLabel string) (*httptest.Server, *fakeGateway, *fakeProm) {
	gw := &fakeGateway{cluster: "main"}
	gw.valid.Store("")
	gws := httptest.NewServer(gw)
	t.Cleanup(gws.Close)
	prom := &fakeProm{}
	ps := httptest.NewServer(prom)
	t.Cleanup(ps.Close)
	u, _ := url.Parse(gws.URL)
	s := New(Config{
		Deployment: "dev", Region: "us-east-1", Pool: "pool1", Pools: []string{"pool1"}, Cluster: "main",
		Gateway: u.Host, Prometheus: ps.URL, PoolLabel: poolLabel,
		Nodes: func(context.Context) ([]awsx.Node, error) {
			return []awsx.Node{
				{InstanceID: "i-a", PrivateIP: "10.0.1.10", State: "running", SSM: "Online", Provision: "Success"},
				{InstanceID: "i-b", PrivateIP: "10.0.1.11", State: "running", SSM: "Online", Provision: "Pending"},
			}, nil
		},
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv, gw, prom
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

func TestLoginAndOverview(t *testing.T) {
	srv, gw, _ := newTestServer(t, "")
	c := &client{t: t, base: srv.URL}

	if code, _ := c.do("GET", "/api/overview", ""); code != 401 {
		t.Fatalf("overview before login: %d, want 401", code)
	}
	if code, out := c.do("POST", "/api/login", `{"username":"admin","password":"nope"}`); code != 401 || !strings.Contains(out["error"].(string), "wrong login") {
		t.Fatalf("bad password: %d %v", code, out)
	}
	if code, out := c.do("POST", "/api/login", `{"username":"admin","password":"secret"}`); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}

	code, out := c.do("GET", "/api/overview", "")
	if code != 200 {
		t.Fatalf("overview: %d %v", code, out)
	}
	if out["vip"] != "10.0.1.100" || out["warnings"] != nil {
		t.Errorf("vip/warnings: %v %v", out["vip"], out["warnings"])
	}
	nodes := out["nodes"].([]any)
	if len(nodes) != 2 {
		t.Fatalf("nodes: %v", nodes)
	}
	a, b := nodes[0].(map[string]any), nodes[1].(map[string]any)
	if a["hostname"] != "node-a" || a["instance_id"] != "i-a" || a["status"] != "UP" || a["cpu"] != 12.5 || a["ram"] != 40.0 {
		t.Errorf("node a joined wrong: %v", a)
	}
	if s := a["services"].(map[string]any); s["core"] != true || s["cache"] != false {
		t.Errorf("node a services: %v", s)
	}
	if s := b["services"].(map[string]any); b["status"] != "DOWN" || s["core"] != false {
		t.Errorf("node b (federated, relabelled): %v", b)
	}

	// An expired gateway token is renewed with the session's login.
	gw.expire()
	if code, _ := c.do("GET", "/api/overview", ""); code != 200 || gw.logins.Load() != 2 {
		t.Errorf("after expiry: %d, logins %d", code, gw.logins.Load())
	}

	c.do("POST", "/api/logout", "")
	if code, _ := c.do("GET", "/api/overview", ""); code != 401 {
		t.Errorf("after logout: %d, want 401", code)
	}
}

func TestSeries(t *testing.T) {
	srv, _, prom := newTestServer(t, "pool1")
	c := &client{t: t, base: srv.URL}
	c.do("POST", "/api/login", `{"username":"admin","password":"secret"}`)

	if code, _ := c.do("GET", "/api/series?metric=up", ""); code != 400 {
		t.Errorf("unknown metric: %d, want 400", code)
	}
	code, out := c.do("GET", "/api/series?metric=cpu&range=900", "")
	if code != 200 {
		t.Fatalf("series: %d %v", code, out)
	}
	ts := out["t"].([]any)
	if len(ts) != 61 { // 900s at the 15s floor, both ends included
		t.Errorf("points: %d, want 61", len(ts))
	}
	lines := out["series"].([]any)
	first := lines[0].(map[string]any)
	if first["ip"] != "10.0.1.10" || first["v"].([]any)[0] != 7.0 || first["v"].([]any)[1] != nil {
		t.Errorf("aligned series: %v", first)
	}
	// Under mgmt every query is limited to the pool.
	for _, q := range prom.queries {
		if !strings.Contains(q, `pool="pool1"`) {
			t.Errorf("query without the pool label: %s", q)
		}
	}
}

func TestLocalOnly(t *testing.T) {
	srv, _, _ := newTestServer(t, "")
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
