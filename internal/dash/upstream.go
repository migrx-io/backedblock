package dash

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// errUnauthorized means the browser has no session: log in.
var errUnauthorized = errors.New("not logged in")

// gateway is the cluster's management API (mgx-gateway-api on :8082). The
// dashboard uses it only to check a login.
type gateway struct {
	addr    string // host:port
	cluster string

	mu     sync.Mutex
	scheme string // detected on first use; the gateway serves HTTPS only when it has a certificate
}

var (
	plainClient = &http.Client{Timeout: 20 * time.Second}
	// The gateway's certificate is self-signed, and the connection is to the
	// local end of an SSM tunnel, which is already encrypted.
	tlsClient = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see above
	}}
)

// do sends a request, trying HTTP and then HTTPS until one gets a response.
func (g *gateway) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	g.mu.Lock()
	schemes := []string{g.scheme}
	if g.scheme == "" {
		schemes = []string{"http", "https"}
	}
	g.mu.Unlock()
	var lastErr error
	for _, s := range schemes {
		req, err := http.NewRequestWithContext(ctx, method, s+"://"+g.addr+path, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		c := plainClient
		if s == "https" {
			c = tlsClient
		}
		resp, err := c.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		g.mu.Lock()
		g.scheme = s
		g.mu.Unlock()
		return resp, nil
	}
	return nil, fmt.Errorf("cluster API: %w", lastErr)
}

// login returns a gateway access token for the user.
func (g *gateway) login(ctx context.Context, user, password string) (string, error) {
	resp, err := g.do(ctx, http.MethodPost, "/api/v1/auth", map[string]string{
		"cluster": g.cluster, "ns": g.cluster, "username": user, "password": password,
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		Error       any    `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	switch {
	case resp.StatusCode == http.StatusAccepted:
		return "", errors.New("this account uses two-factor login, which the dashboard does not support yet")
	case resp.StatusCode != http.StatusOK || out.AccessToken == "":
		if out.Error != nil {
			return "", fmt.Errorf("login failed: %v", out.Error)
		}
		return "", fmt.Errorf("login failed: %s", resp.Status)
	}
	return out.AccessToken, nil
}

// prometheus is the read-only query API of a node's Prometheus (:9090).
type prometheus struct {
	base string // http://host:port
}

type sample struct {
	Labels map[string]string
	Value  float64
}

type series struct {
	Labels map[string]string
	Points [][2]float64 // unix seconds, value
}

func (p *prometheus) api(ctx context.Context, path string, q url.Values, data any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := plainClient.Do(req)
	if err != nil {
		return fmt.Errorf("prometheus: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("prometheus: %s", resp.Status)
	}
	if out.Status != "success" {
		return fmt.Errorf("prometheus: %s", out.Error)
	}
	return json.Unmarshal(out.Data, data)
}

func parseValue(v [2]any) (float64, float64, bool) {
	t, ok := v[0].(float64)
	s, ok2 := v[1].(string)
	if !ok || !ok2 {
		return 0, 0, false
	}
	// NaN and ±Inf (a 0/0 ratio, say) are dropped: JSON cannot carry them.
	f, err := strconv.ParseFloat(s, 64)
	return t, f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// query evaluates an instant vector expression.
func (p *prometheus) query(ctx context.Context, expr string) ([]sample, error) {
	var d struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	}
	if err := p.api(ctx, "/api/v1/query", url.Values{"query": {expr}}, &d); err != nil {
		return nil, err
	}
	out := make([]sample, 0, len(d.Result))
	for _, r := range d.Result {
		if _, v, ok := parseValue(r.Value); ok {
			out = append(out, sample{r.Metric, v})
		}
	}
	return out, nil
}

// queryRange evaluates expr over [start, end] every step.
func (p *prometheus) queryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) ([]series, error) {
	var d struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		} `json:"result"`
	}
	q := url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.Itoa(int(step.Seconds()))},
	}
	if err := p.api(ctx, "/api/v1/query_range", q, &d); err != nil {
		return nil, err
	}
	out := make([]series, 0, len(d.Result))
	for _, r := range d.Result {
		s := series{Labels: r.Metric}
		for _, v := range r.Values {
			if t, f, ok := parseValue(v); ok {
				s.Points = append(s.Points, [2]float64{t, f})
			}
		}
		out = append(out, s)
	}
	return out, nil
}
