// Package dash is the local dashboard behind `bbctl ui`: a web server on
// 127.0.0.1 that shows what the clusters' Prometheus holds, the data behind
// the mgx-metrics Grafana dashboards. The cluster API is used only to check
// the login. Both are reached through SSM port forwards that the caller sets
// up; nothing comes from bbctl's config or AWS.
package dash

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var web embed.FS

// Config is where the dashboard reads from.
type Config struct {
	// Cluster is the cluster (and namespace) to log in to.
	Cluster string
	// Gateway is the host:port of a cluster API, used to check logins.
	Gateway string
	// LoginHint tells the login page where the password is.
	LoginHint string
	// Pools are the pools to show.
	Pools []Pool
}

// Pool is where one pool's series are.
type Pool struct {
	Name string
	// Prometheus is the base URL of a Prometheus holding the pool's series;
	// empty when the pool has no metrics.
	Prometheus string
	// Label limits series to the pool. It is set when Prometheus is the
	// management plane's, which holds every pool's series labelled
	// pool="<name>"; a pool's own Prometheus holds only that pool.
	Label string
}

type pool struct {
	name  string
	prom  *prometheus
	label string
}

// Server serves the page and its JSON API.
type Server struct {
	cfg   Config
	gw    *gateway
	pools []*pool

	mu       sync.Mutex
	sessions map[string]bool
}

const cookieName = "bbctl_ui"

func New(cfg Config) *Server {
	s := &Server{cfg: cfg, gw: &gateway{addr: cfg.Gateway, cluster: cfg.Cluster}, sessions: map[string]bool{}}
	for _, p := range cfg.Pools {
		pp := &pool{name: p.Name, label: p.Label}
		if p.Prometheus != "" {
			pp.prom = &prometheus{base: p.Prometheus}
		}
		s.pools = append(s.pools, pp)
	}
	return s
}

// Handler returns the routes: the static page and /api/*.
func (s *Server) Handler() http.Handler {
	static, _ := fs.Sub(web, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/info", s.info)
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.logout)
	mux.HandleFunc("GET /api/overview", s.authed(s.overview))
	mux.HandleFunc("GET /api/series", s.authed(s.series))
	mux.HandleFunc("GET /api/cache", s.authed(s.cache))
	mux.HandleFunc("GET /api/volumes", s.authed(s.storage))
	mux.HandleFunc("GET /api/volume/series", s.authed(s.volumeSeries))
	mux.HandleFunc("GET /api/snapshots", s.authed(s.snapshots))
	mux.HandleFunc("GET /api/host/quick", s.authed(s.hostQuick))
	mux.HandleFunc("GET /api/host/series", s.authed(s.hostSeries))
	return localOnly(mux)
}

// localOnly refuses requests whose Host is not a loopback address, so a web
// page elsewhere cannot reach the API through DNS rebinding.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) poolNames() []string {
	names := make([]string, len(s.pools))
	for i, p := range s.pools {
		names[i] = p.name
	}
	return names
}

// info is what the page needs before there is a session.
func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"cluster": s.cfg.Cluster, "login_hint": s.cfg.LoginHint, "pools": s.poolNames()})
}

// login checks the user against the cluster API and starts a session.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&in) != nil || in.Username == "" {
		writeError(w, http.StatusBadRequest, errors.New("username and password are required"))
		return
	}
	if _, err := s.gw.login(r.Context(), in.Username, in.Password); err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[id] = true
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: id, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]string{"user": in.Username})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ok := false
		if c, err := r.Cookie(cookieName); err == nil {
			s.mu.Lock()
			ok = s.sessions[c.Value]
			s.mu.Unlock()
		}
		if !ok {
			writeError(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		h(w, r)
	}
}

// selected returns the pools a request asks for: ?pool=a,b, or every pool
// for "all" or none. They come back in the configured order.
func (s *Server) selected(r *http.Request) ([]*pool, error) {
	q := r.URL.Query().Get("pool")
	if q == "" || q == "all" {
		return s.pools, nil
	}
	want := map[string]bool{}
	for _, name := range strings.Split(q, ",") {
		if name = strings.TrimSpace(name); name != "" {
			want[name] = true
		}
	}
	var out []*pool
	for _, p := range s.pools {
		if want[p.name] {
			out = append(out, p)
			delete(want, p.name)
		}
	}
	for name := range want {
		return nil, fmt.Errorf("no pool %q", name)
	}
	if len(out) == 0 {
		return s.pools, nil
	}
	return out, nil
}

// each runs f for every pool in parallel and waits.
func each(pools []*pool, f func(i int, p *pool)) {
	var wg sync.WaitGroup
	for i, p := range pools {
		wg.Add(1)
		go func() { defer wg.Done(); f(i, p) }()
	}
	wg.Wait()
}

// warn prefixes an error with its pool when there is more than one.
func (s *Server) warn(p *pool, err error) string {
	if len(s.pools) > 1 {
		return p.name + ": " + err.Error()
	}
	return err.Error()
}

var errNoMetrics = errors.New("no metrics: enable_metrics is off")

// selector is the pool's label matcher, empty or `pool="x"`.
func (p *pool) selector() string {
	if p.label == "" {
		return ""
	}
	return `pool="` + p.label + `"`
}

// matchers joins label matchers for a selector's braces.
func matchers(m ...string) string {
	var out []string
	for _, s := range m {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ",")
}

// NodeView is one row of the node table.
type NodeView struct {
	Pool     string `json:"pool"`
	Hostname string `json:"hostname,omitempty"`
	IP       string `json:"ip"`
	UUID     string `json:"uuid,omitempty"`
	// Status is what the node's cluster reports for it: UP, DOWN, or ""
	// when the node publishes no core_cluster_nodes series.
	Status string `json:"status"`
	// Services maps core, cache and storage to whether Prometheus could
	// scrape them.
	Services map[string]bool `json:"services,omitempty"`
	// Kernel is the node's, when node_exporter reports it.
	Kernel string `json:"kernel,omitempty"`
}

// Overview serves the Overview view, the mgx-metrics "Core" dashboard's node
// health, and the Hosts view, each node's node_exporter readings.
type Overview struct {
	Cluster string     `json:"cluster"`
	Pools   []string   `json:"pools"`
	Nodes   []NodeView `json:"nodes"`
	// Warnings are sources that failed; the rest of the view still shows.
	Warnings []string `json:"warnings,omitempty"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	pools, err := s.selected(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	rows := make([][]NodeView, len(pools))
	errs := make([]error, len(pools))
	each(pools, func(i int, p *pool) { rows[i], errs[i] = p.overview(ctx) })
	ov := Overview{Cluster: s.cfg.Cluster, Pools: s.poolNames(), Nodes: []NodeView{}}
	for i, p := range pools {
		if errs[i] != nil {
			ov.Warnings = append(ov.Warnings, s.warn(p, errs[i]))
		}
		ov.Nodes = append(ov.Nodes, rows[i]...)
	}
	writeJSON(w, http.StatusOK, ov)
}

// overview is the pool's node rows. Each node publishes its own
// core_cluster_nodes series (hostname, ip, uuid, status), so the rows are
// those, ordered by hostname, then any node only `up` or node_exporter
// knows: one that stopped reporting.
func (p *pool) overview(ctx context.Context) ([]NodeView, error) {
	if p.prom == nil {
		return nil, errNoMetrics
	}
	sel := p.selector()
	var members, up []sample
	var hosts []Host
	var errs [3]error
	var wg sync.WaitGroup
	for i, q := range []struct {
		dst  *[]sample
		expr string
	}{
		{&members, "core_cluster_nodes{" + sel + "}"},
		{&up, "up{" + matchers(`job=~"core|cache|storage"`, sel) + "}"},
	} {
		wg.Add(1)
		go func() { defer wg.Done(); *q.dst, errs[i] = p.prom.query(ctx, q.expr) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); hosts, errs[2] = p.hosts(ctx) }()
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}

	sort.Slice(members, func(i, j int) bool { return members[i].Labels["hostname"] < members[j].Labels["hostname"] })
	byIP := map[string]*NodeView{}
	var order []string
	row := func(ip string) *NodeView {
		if n, ok := byIP[ip]; ok {
			return n
		}
		n := &NodeView{Pool: p.name, IP: ip}
		byIP[ip] = n
		order = append(order, ip)
		return n
	}
	for _, m := range members {
		if m.Labels["ip"] == "" {
			continue
		}
		v := row(m.Labels["ip"])
		v.Hostname, v.UUID, v.Status = m.Labels["hostname"], m.Labels["uuid"], m.Labels["status"]
	}
	for _, smp := range up {
		ip := scrapedHost(smp.Labels)
		if ip == "" {
			continue
		}
		v := row(ip)
		if v.Services == nil {
			v.Services = map[string]bool{}
		}
		v.Services[smp.Labels["job"]] = smp.Value == 1
	}
	for _, h := range hosts {
		v := row(h.IP)
		v.Kernel = h.Kernel
		if v.Hostname == "" {
			v.Hostname = h.Hostname
		}
	}
	rows := make([]NodeView, 0, len(order))
	for _, ip := range order {
		rows = append(rows, *byIP[ip])
	}
	return rows, nil
}

// hostnames maps node UUIDs to hostnames, from core_cluster_nodes.
func (p *pool) hostnames(ctx context.Context) (map[string]string, error) {
	res, err := p.prom.query(ctx, "core_cluster_nodes{"+p.selector()+"}")
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(res))
	for _, r := range res {
		m[r.Labels["uuid"]] = r.Labels["hostname"]
	}
	return m, nil
}

// scrapedHost is the node an up sample is about. Pool nodes scrape peers as
// <ip>:8082; through mgmt federation a node's own localhost target is
// relabelled with its IP (instance=<ip>, or node=<ip>).
func scrapedHost(l map[string]string) string {
	host := l["instance"]
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || host == "localhost" || host == "127.0.0.1" {
		host = l["node"]
	}
	if net.ParseIP(host) == nil {
		return ""
	}
	return host
}

// charts are the series the page may ask for, by name, each line a node by
// its ip label: the page never sends PromQL, so the API cannot be used to run
// arbitrary queries. cpu and ram are the "Core" dashboard's, as each node's
// core reports them.
var charts = map[string]string{
	"cpu": "core_cluster_system_cpu{{sel}}",
	"ram": "core_cluster_system_ram{{sel}}",
}

// Series is one chart's data, aligned for uPlot: one shared time axis and a
// value (or null) per series at each time.
type Series struct {
	Times  []int64 `json:"t"`
	Series []Line  `json:"series"`
	// Unit is a panel's unit (see panel), for the page to print values;
	// Zero counts its lines left out for staying at zero.
	Unit     string   `json:"unit,omitempty"`
	Zero     int      `json:"zero,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Line is a node's series (IP set) or a panel's (Label set, and Volume on a
// volume panel).
type Line struct {
	Pool   string     `json:"pool"`
	IP     string     `json:"ip,omitempty"`
	Volume string     `json:"volume,omitempty"`
	Label  string     `json:"label,omitempty"`
	Values []*float64 `json:"v"`
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	pools, err := s.selected(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	expr, ok := charts[r.URL.Query().Get("metric")]
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown metric"))
		return
	}
	start, end, step := window(r)

	res := make([][]series, len(pools))
	errs := make([]error, len(pools))
	each(pools, func(i int, p *pool) {
		if p.prom == nil {
			errs[i] = errNoMetrics
			return
		}
		// A rate needs at least four scrapes, and must span the step so no
		// sample falls between points.
		res[i], errs[i] = p.prom.queryRange(r.Context(), expand(expr, p.selector(), max(step, time.Minute)), start, end, step)
	})
	out := grid(start, end, step)
	for i, p := range pools {
		if errs[i] != nil {
			out.Warnings = append(out.Warnings, s.warn(p, errs[i]))
			continue
		}
		out.add(p.name, res[i])
	}
	writeJSON(w, http.StatusOK, out)
}

// window is a chart's time range from ?range= (seconds, an hour by default)
// and its step: about 300 points, never finer than the 15s scrape interval.
func window(r *http.Request) (start, end time.Time, step time.Duration) {
	rng, err := strconv.Atoi(r.URL.Query().Get("range"))
	if err != nil || rng < 60 || rng > 7*24*3600 {
		rng = 3600
	}
	end = time.Now().Truncate(time.Second)
	start = end.Add(-time.Duration(rng) * time.Second)
	step = max(time.Duration(rng)*time.Second/300, 15*time.Second).Truncate(time.Second)
	return start, end, step
}

// grid is an empty chart on the time grid start + k*step.
func grid(start, end time.Time, step time.Duration) *Series {
	out := &Series{Series: []Line{}}
	for t := start.Unix(); t <= end.Unix(); t += int64(step.Seconds()) {
		out.Times = append(out.Times, t)
	}
	return out
}

// add puts a pool's node series on the chart's grid, ordered by IP.
func (out *Series) add(poolName string, res []series) {
	sort.Slice(res, func(i, j int) bool { return res[i].Labels["ip"] < res[j].Labels["ip"] })
	for _, sr := range res {
		if ip := sr.Labels["ip"]; ip != "" {
			out.addLine(Line{Pool: poolName, IP: ip}, sr)
		}
	}
}

// addLine puts one series on the chart's grid as line.
func (out *Series) addLine(line Line, sr series) {
	line.Values = make([]*float64, len(out.Times))
	step := int64(1)
	if len(out.Times) > 1 {
		step = out.Times[1] - out.Times[0]
	}
	for _, pt := range sr.Points {
		if t := int64(pt[0]); len(out.Times) > 0 && (t-out.Times[0])%step == 0 {
			if i := int((t - out.Times[0]) / step); i >= 0 && i < len(out.Times) {
				v := pt[1]
				line.Values[i] = &v
			}
		}
	}
	out.Series = append(out.Series, line)
}
