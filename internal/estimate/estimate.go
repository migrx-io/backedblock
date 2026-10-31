// Package estimate calls the cost calculator on backedblock.io. The model
// lives on the site (/api/estimate runs the same code as the page), so bbctl
// sends inputs and reads the answer rather than keeping a copy of the formula.
package estimate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// URL is the calculator API. BBCTL_ESTIMATE_URL overrides it, e.g. for a
// local `make dev` of the site.
const URL = "https://backedblock.io/api/estimate"

// Inputs are calculator inputs by their API names (volumeCount, volumeGB,
// rwMBps, iops, arch, ...). Missing ones take the calculator's defaults.
type Inputs map[string]any

type Result struct {
	Note          string            `json:"note"`
	RateRegion    string            `json:"rateRegion"`
	CalculatorURL string            `json:"calculatorUrl"`
	Inputs        map[string]any    `json:"inputs"`
	Plans         []Plan            `json:"plans"`
	Savings       *Savings          `json:"savings"`
	Sizing        Sizing            `json:"sizing"`
	Split         *Split            `json:"split"`
	Notices       map[string]string `json:"notices"`
	// Config is one pool's bbctl.yaml fields and the StorageClass parameters,
	// as YAML text. Null when nothing in the catalogue fits.
	Config *Config `json:"config"`
}

type Plan struct {
	ID    string  `json:"id"`
	Name  string  `json:"name"`
	Items []Item  `json:"items"`
	Total float64 `json:"total"`
}

type Item struct {
	Label   string  `json:"label"`
	Detail  string  `json:"detail"`
	Monthly float64 `json:"monthly"`
}

type Savings struct {
	Factor   float64 `json:"factor"`
	Percent  float64 `json:"percent"`
	Absolute float64 `json:"absolute"`
}

// Sizing is the part of the site's NodeSizing bbctl reads; the rest is kept
// in Result only through the JSON it was decoded from.
type Sizing struct {
	Nodes     int       `json:"nodes"`
	Instance  *Instance `json:"instance"`
	Shortfall *string   `json:"shortfall"`
	Monthly   float64   `json:"monthly"`
}

type Instance struct {
	API       string  `json:"api"`
	Arch      string  `json:"arch"`
	VCPU      int     `json:"vcpu"`
	MemoryGiB float64 `json:"memoryGiB"`
	Hourly    float64 `json:"hourly"`
}

// Split is set when the workload needs several pools of this sizing.
type Split struct {
	Pools          int    `json:"pools"`
	VolumesPerPool int    `json:"volumesPerPool"`
	Sizing         Sizing `json:"sizing"`
}

type Config struct {
	Pool         string `json:"pool"`
	StorageClass string `json:"storageClass"`
}

// InputError is the calculator rejecting the inputs; Details lists each one.
type InputError struct {
	Message string   `json:"error"`
	Details []string `json:"details"`
}

func (e *InputError) Error() string { return e.Message }

// Plan returns the plan with the given ID: "backedblock" or "gp3".
func (r *Result) Plan(id string) (Plan, bool) {
	for _, p := range r.Plans {
		if p.ID == id {
			return p, true
		}
	}
	return Plan{}, false
}

// Run prices the inputs. Nil inputs return the calculator's defaults, which
// is how bbctl learns them.
func Run(ctx context.Context, in Inputs) (*Result, error) {
	url := URL
	if u := os.Getenv("BBCTL_ESTIMATE_URL"); u != "" {
		url = u
	}
	if in == nil {
		in = Inputs{}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling the calculator: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest {
		e := &InputError{}
		if json.NewDecoder(resp.Body).Decode(e) == nil && e.Message != "" {
			return nil, e
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("calling the calculator: POST %s: %s", url, resp.Status)
	}
	var r Result
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("parsing the calculator's answer: %w", err)
	}
	return &r, nil
}
