package dash

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// The mock must keep answering every query the page's views send, at the
// size asked for.
func TestMock(t *testing.T) {
	cfg, stop, err := StartMock(MockSize{Pools: 2, Nodes: 3, Volumes: 4, Snapshots: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	srv := httptest.NewServer(New(cfg).Handler())
	defer srv.Close()
	c := &client{t: t, base: srv.URL}
	if code, _ := c.do("POST", "/api/login", `{"username":"admin","password":"anything"}`); code != 200 {
		t.Fatalf("login: %d", code)
	}
	counts := map[string]struct {
		key  string
		want int
	}{
		"/api/overview?pool=all":  {"nodes", 6},
		"/api/cache?pool=all":     {"nodes", 6},
		"/api/volumes?pool=all":   {"volumes", 24},
		"/api/snapshots?pool=all": {"snapshots", 30},
	}
	for path, w := range counts {
		code, out := c.do("GET", path, "")
		if got, _ := out[w.key].([]any); code != 200 || len(got) != w.want || out["warnings"] != nil {
			t.Errorf("%s: %d, %d %s, want %d (%v)", path, code, len(got), w.key, w.want, out["warnings"])
		}
	}
	_, vols := c.do("GET", "/api/volumes?pool=all", "")
	v := vols["volumes"].([]any)[5].(map[string]any)
	pick := v["pool"].(string) + "/" + v["name"].(string)
	_, out := c.do("GET", "/api/volume/series?panel=spdk_write&range=3600&pool=all&vols="+pick, "")
	if lines := out["series"].([]any); len(lines) != 1 || lines[0].(map[string]any)["volume"] != v["name"] {
		t.Errorf("picked volume's chart: %v", out["series"])
	}
	ip := strings.TrimSpace(vols["volumes"].([]any)[0].(map[string]any)["ip"].(string))
	if _, out := c.do("GET", "/api/series?metric=cpu&range=3600&pool=all&nodes=pool1/"+ip, ""); len(out["series"].([]any)) != 1 {
		t.Errorf("picked node's chart: %v", out["series"])
	}
	if _, q := c.do("GET", "/api/host/quick?pool=pool1&ip="+ip, ""); q["cpu_cores"] != 16.0 {
		t.Errorf("host quick: %v", q)
	}
	if _, out := c.do("GET", "/api/host/series?pool=pool1&ip="+ip+"&panel=storage_disk_iops&range=3600", ""); len(out["series"].([]any)) == 0 {
		t.Errorf("host series: %v", out)
	}
}
