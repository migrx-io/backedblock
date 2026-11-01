// Package dash is the local dashboard behind `bbctl ui`: a web server on
// 127.0.0.1 that logs in to a pool's cluster API and reads Prometheus, both
// reached through SSM port forwards that the caller sets up.
package dash

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/migrx-io/backedblock/internal/awsx"
)

//go:embed web
var web embed.FS

// Config is what the dashboard shows and where it reads it from.
type Config struct {
	Deployment string
	Region     string
	Pool       string
	Pools      []string
	// Cluster is the cluster (and namespace) to log in to.
	Cluster string
	// Gateway is the host:port of the forwarded cluster API.
	Gateway string
	// Prometheus is the base URL of the forwarded Prometheus; empty when the
	// pool has no metrics.
	Prometheus string
	// PoolLabel limits Prometheus series to the pool. It is set when
	// Prometheus is the management plane's, which holds every pool's series
	// labelled pool="<name>".
	PoolLabel string
	// Nodes returns the pool's nodes as EC2 and SSM see them, in node order.
	Nodes func(ctx context.Context) ([]awsx.Node, error)
}

// Server serves the page and its JSON API.
type Server struct {
	cfg  Config
	gw   *gateway
	prom *prometheus

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	user, password, token string
}

const cookieName = "bbctl_ui"

func New(cfg Config) *Server {
	s := &Server{cfg: cfg, gw: &gateway{addr: cfg.Gateway, cluster: cfg.Cluster}, sessions: map[string]*session{}}
	if cfg.Prometheus != "" {
		s.prom = &prometheus{base: cfg.Prometheus}
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

// info is what the login page shows before there is a session.
func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"deployment": s.cfg.Deployment, "pool": s.cfg.Pool, "cluster": s.cfg.Cluster})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&in) != nil || in.Username == "" {
		writeError(w, http.StatusBadRequest, errors.New("username and password are required"))
		return
	}
	token, err := s.gw.login(r.Context(), in.Username, in.Password)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err)
		return
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	s.mu.Lock()
	// The password is kept to log in again when the gateway's token expires.
	s.sessions[id] = &session{user: in.Username, password: in.Password, token: token}
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

type ctxKey struct{}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var sess *session
		if c, err := r.Cookie(cookieName); err == nil {
			s.mu.Lock()
			sess = s.sessions[c.Value]
			s.mu.Unlock()
		}
		if sess == nil {
			writeError(w, http.StatusUnauthorized, errUnauthorized)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess)))
	}
}

// gatewayGet calls the cluster API with the session's token, logging in
// again once if the token has expired.
func (s *Server) gatewayGet(ctx context.Context, path string, v any) error {
	sess := ctx.Value(ctxKey{}).(*session)
	s.mu.Lock()
	token := sess.token
	s.mu.Unlock()
	err := s.gw.get(ctx, token, path, v)
	if !errors.Is(err, errUnauthorized) {
		return err
	}
	if token, err = s.gw.login(ctx, sess.user, sess.password); err != nil {
		return errUnauthorized
	}
	s.mu.Lock()
	sess.token = token
	s.mu.Unlock()
	return s.gw.get(ctx, token, path, v)
}

// NodeView is one row of the node table.
type NodeView struct {
	Hostname   string `json:"hostname,omitempty"`
	IP         string `json:"ip"`
	UUID       string `json:"uuid,omitempty"`
	Status     string `json:"status"` // the cluster's view: UP, DOWN, or "" if it does not list the node
	InstanceID string `json:"instance_id,omitempty"`
	AZ         string `json:"az,omitempty"`
	Type       string `json:"instance_type,omitempty"`
	State      string `json:"state,omitempty"`
	SSM        string `json:"ssm,omitempty"`
	Provision  string `json:"provision,omitempty"`
	// Services maps core, cache and storage to whether Prometheus could
	// scrape them; missing when there are no metrics for the node.
	Services map[string]bool `json:"services,omitempty"`
	CPU      *float64        `json:"cpu,omitempty"`
	RAM      *float64        `json:"ram,omitempty"`
}

// Overview is the main view.
type Overview struct {
	Deployment string     `json:"deployment"`
	Region     string     `json:"region"`
	Pool       string     `json:"pool"`
	Pools      []string   `json:"pools"`
	Cluster    string     `json:"cluster"`
	VIP        string     `json:"vip,omitempty"`
	VIPNode    string     `json:"vip_node,omitempty"`
	Metrics    bool       `json:"metrics"`
	Nodes      []NodeView `json:"nodes"`
	// Warnings are sources that failed; the rest of the view still shows.
	Warnings []string `json:"warnings,omitempty"`
}

var services = []string{"core", "cache", "storage"}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	ov := Overview{Deployment: s.cfg.Deployment, Region: s.cfg.Region, Pool: s.cfg.Pool, Pools: s.cfg.Pools, Cluster: s.cfg.Cluster, Metrics: s.prom != nil}

	var (
		wg                  sync.WaitGroup
		ec2                 []awsx.Node
		clusters            []clusterInfo
		members             []clusterNode
		up, cpu, ram        []sample
		ec2Err, gwErr, mErr error
	)
	wg.Add(2)
	go func() { defer wg.Done(); ec2, ec2Err = s.cfg.Nodes(ctx) }()
	go func() {
		defer wg.Done()
		if gwErr = s.gatewayGet(ctx, "/api/v1/cluster", &clusters); gwErr == nil {
			gwErr = s.gatewayGet(ctx, "/api/v1/cluster/"+s.cfg.Cluster+"/nodes", &members)
		}
	}()
	if s.prom != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sel := s.selector()
			if up, mErr = s.prom.query(ctx, `up{job=~"core|cache|storage"`+sel+`}`); mErr != nil {
				return
			}
			if cpu, mErr = s.prom.query(ctx, `core_cluster_system_cpu{`+strings.TrimPrefix(sel, ",")+`}`); mErr != nil {
				return
			}
			ram, mErr = s.prom.query(ctx, `core_cluster_system_ram{`+strings.TrimPrefix(sel, ",")+`}`)
		}()
	}
	wg.Wait()

	if errors.Is(gwErr, errUnauthorized) {
		writeError(w, http.StatusUnauthorized, gwErr)
		return
	}
	for _, e := range []error{ec2Err, gwErr, mErr} {
		if e != nil {
			ov.Warnings = append(ov.Warnings, e.Error())
		}
	}

	// Rows come from EC2 in node order, then any node the cluster lists that
	// EC2 does not (it should not happen, but it should not be hidden).
	byIP := map[string]*NodeView{}
	var order []string
	row := func(ip string) *NodeView {
		if n, ok := byIP[ip]; ok {
			return n
		}
		n := &NodeView{IP: ip}
		byIP[ip] = n
		order = append(order, ip)
		return n
	}
	for _, n := range ec2 {
		v := row(n.PrivateIP)
		v.InstanceID, v.AZ, v.Type, v.State, v.SSM, v.Provision = n.InstanceID, n.AZ, n.Type, n.State, n.SSM, n.Provision
	}
	for _, m := range members {
		v := row(m.IP)
		v.Hostname, v.UUID, v.Status = m.Hostname, m.UUID, m.Status
	}
	for _, c := range clusters {
		if c.Cluster == s.cfg.Cluster {
			ov.VIP, ov.VIPNode = c.VIPHost.VIP, c.VIPHost.IP
		}
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
	for _, smp := range cpu {
		if ip := smp.Labels["ip"]; ip != "" {
			val := smp.Value
			row(ip).CPU = &val
		}
	}
	for _, smp := range ram {
		if ip := smp.Labels["ip"]; ip != "" {
			val := smp.Value
			row(ip).RAM = &val
		}
	}
	ov.Nodes = make([]NodeView, 0, len(order))
	for _, ip := range order {
		ov.Nodes = append(ov.Nodes, *byIP[ip])
	}
	writeJSON(w, http.StatusOK, ov)
}

// selector is the extra label matcher for every query, with a leading comma.
func (s *Server) selector() string {
	if s.cfg.PoolLabel == "" {
		return ""
	}
	return `,pool="` + s.cfg.PoolLabel + `"`
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

// charts are the series the page may ask for, by name: the page never sends
// PromQL, so the API cannot be used to run arbitrary queries.
var charts = map[string]string{
	"cpu": "core_cluster_system_cpu",
	"ram": "core_cluster_system_ram",
}

// Series is one chart's data, aligned for uPlot: one shared time axis and a
// value (or null) per series at each time.
type Series struct {
	Times  []int64    `json:"t"`
	Series []NodeLine `json:"series"`
}

type NodeLine struct {
	IP     string     `json:"ip"`
	Values []*float64 `json:"v"`
}

func (s *Server) series(w http.ResponseWriter, r *http.Request) {
	if s.prom == nil {
		writeError(w, http.StatusNotFound, errors.New("this pool has no metrics (enable_metrics is off)"))
		return
	}
	metric, ok := charts[r.URL.Query().Get("metric")]
	if !ok {
		writeError(w, http.StatusBadRequest, errors.New("unknown metric"))
		return
	}
	rng, err := strconv.Atoi(r.URL.Query().Get("range"))
	if err != nil || rng < 60 || rng > 7*24*3600 {
		rng = 3600
	}
	end := time.Now().Truncate(time.Second)
	start := end.Add(-time.Duration(rng) * time.Second)
	// About 300 points per chart, never finer than the 15s scrape interval.
	step := max(time.Duration(rng)*time.Second/300, 15*time.Second).Truncate(time.Second)
	res, err := s.prom.queryRange(r.Context(), metric+"{"+strings.TrimPrefix(s.selector(), ",")+"}", start, end, step)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, align(res, start, end, step))
}

// align puts every series on the same time grid, start + k*step.
func align(res []series, start, end time.Time, step time.Duration) Series {
	var out Series
	idx := map[int64]int{}
	for t := start.Unix(); t <= end.Unix(); t += int64(step.Seconds()) {
		idx[t] = len(out.Times)
		out.Times = append(out.Times, t)
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Labels["ip"] < res[j].Labels["ip"] })
	for _, sr := range res {
		line := NodeLine{IP: sr.Labels["ip"], Values: make([]*float64, len(out.Times))}
		for _, p := range sr.Points {
			if i, ok := idx[int64(p[0])]; ok {
				v := p[1]
				line.Values[i] = &v
			}
		}
		out.Series = append(out.Series, line)
	}
	return out
}
