package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/render"
	"github.com/migrx-io/backedblock/internal/tf"
	"github.com/migrx-io/backedblock/internal/ui"
)

// StackResult is the outcome of one stack in plan/apply/destroy.
type StackResult struct {
	ID string `json:"id"`
	// Status: planned, applied, destroyed, unchanged, skipped.
	Status string          `json:"status"`
	Reason string          `json:"reason,omitempty"`
	Plan   *tf.PlanSummary `json:"plan,omitempty"`
}

// Approve decides whether a stack's plan may be applied.
type Approve func(id string, p tf.PlanSummary) error

// Plan plans every stack without changing anything: no state bucket is
// created and no state lock is taken. Stacks that read the network's state
// cannot be planned before the network exists, so they are reported as
// skipped. Before the first apply there is no state bucket at all; the network
// is then planned against an empty local state in a scratch directory.
func (d *Deployment) Plan(ctx context.Context, only map[string]bool) ([]StackResult, error) {
	if d.noState {
		return d.planFresh(ctx, only)
	}
	var res []StackResult
	networkApplied, err := d.applied(ctx, d.Stacks[0])
	if err != nil {
		return nil, err
	}
	for _, s := range d.Stacks {
		if !selected(only, s.ID) {
			continue
		}
		if s.Kind != render.KindNetwork && !networkApplied {
			res = append(res, StackResult{ID: s.ID, Status: "skipped", Reason: "network is not applied yet; planned during apply"})
			continue
		}
		st := ui.Begin(s.ID, "planning", 0)
		r, err := d.Runner(ctx, s)
		if err != nil {
			st.End("", false)
			return res, err
		}
		p, err := r.Plan(ctx, tf.PlanReadOnly)
		st.End("", false)
		if err != nil {
			return res, ui.Wrap(ui.ExitTerraform, "", fmt.Errorf("%s: terraform plan: %w", s.ID, err))
		}
		res = append(res, StackResult{ID: s.ID, Status: "planned", Plan: &p})
	}
	return res, nil
}

// planFresh previews a deployment that was never applied.
func (d *Deployment) planFresh(ctx context.Context, only map[string]bool) ([]StackResult, error) {
	var res []StackResult
	for _, s := range d.Stacks {
		if !selected(only, s.ID) {
			continue
		}
		if s.Kind != render.KindNetwork {
			res = append(res, StackResult{ID: s.ID, Status: "skipped", Reason: "network is not applied yet; planned during apply"})
			continue
		}
		p, err := d.previewNetwork(ctx)
		if err != nil {
			return res, err
		}
		res = append(res, StackResult{ID: s.ID, Status: "planned", Plan: &p,
			Reason: "first deploy: state bucket s3://" + d.Cfg.State.Bucket + " is created by apply"})
	}
	return res, nil
}

// previewNetwork plans the network stack against an empty local state in a
// scratch directory: what a first apply will create, without touching AWS.
func (d *Deployment) previewNetwork(ctx context.Context) (tf.PlanSummary, error) {
	s := d.Stacks[0]
	dir := filepath.Join(d.WorkDir, "preview", s.ID)
	if err := os.RemoveAll(dir); err != nil {
		return tf.PlanSummary{}, err
	}
	if err := render.WriteLocal(d.Cfg, s, d.Stacks, d.inputs, dir); err != nil {
		return tf.PlanSummary{}, err
	}
	st := ui.Begin(s.ID, "planning", 0)
	r, err := d.runnerIn(ctx, s, dir)
	if err != nil {
		st.End("", false)
		return tf.PlanSummary{}, err
	}
	p, err := r.Plan(ctx, tf.PlanReadOnly)
	st.End("", false)
	if err != nil {
		return tf.PlanSummary{}, ui.Wrap(ui.ExitTerraform, "", fmt.Errorf("%s: terraform plan: %w", s.ID, err))
	}
	return p, nil
}

// approveFirstDeploy gets approval for a deployment with no state bucket yet,
// before anything is created: the network preview plus the state bucket. The
// bucket is created only after approval. Returns the approved network plan.
func (d *Deployment) approveFirstDeploy(ctx context.Context, only map[string]bool, approve Approve) (tf.PlanSummary, error) {
	if !selected(only, d.Stacks[0].ID) {
		return tf.PlanSummary{}, ui.Errf(ui.ExitUsage, "run `bbctl apply "+d.Cfg.Name+"` without --stack, or with --stack network, first",
			"nothing is deployed yet: the network must be applied before %v", keys(only))
	}
	ui.Logf("network: planning (first deploy)")
	p, err := d.previewNetwork(ctx)
	if err != nil {
		return p, err
	}
	shown := p
	shown.Add++
	shown.Changes = append([]tf.Change{{Address: "s3://" + d.Cfg.State.Bucket + " (Terraform state bucket)", Action: "create"}}, p.Changes...)
	if err := approve(d.Stacks[0].ID, shown); err != nil {
		return p, err
	}
	created, err := d.AWS.EnsureStateBucket(ctx, d.Cfg.State.Bucket)
	if err != nil {
		return p, ui.Wrap(ui.ExitAWS, "", err)
	}
	if created {
		ui.Logf("created state bucket s3://%s", d.Cfg.State.Bucket)
	}
	d.noState = false
	return p, nil
}

// Apply plans and applies each stack in order, asking approve before any
// stack with changes. Nothing is created before the first approval: on a
// first deploy the state bucket is part of what is approved.
func (d *Deployment) Apply(ctx context.Context, only map[string]bool, approve Approve) ([]StackResult, error) {
	var res []StackResult
	// approved holds a plan already approved for a stack; the real plan is
	// applied without asking again only if it is identical.
	approved := map[string]tf.PlanSummary{}
	if d.noState {
		p, err := d.approveFirstDeploy(ctx, only, approve)
		if err != nil {
			return res, err
		}
		approved[d.Stacks[0].ID] = p
	}
	for _, s := range d.Stacks {
		if !selected(only, s.ID) {
			continue
		}
		ui.Logf("%s: planning", s.ID)
		st := ui.Begin(s.ID, "planning", 0)
		r, err := d.Runner(ctx, s)
		if err != nil {
			st.End("", false)
			return res, err
		}
		p, err := r.Plan(ctx, tf.PlanApply)
		st.End("", false)
		if err != nil {
			return res, ui.Wrap(ui.ExitTerraform, "", fmt.Errorf("%s: terraform plan: %w", s.ID, err))
		}
		if p.Empty() {
			res = append(res, StackResult{ID: s.ID, Status: "unchanged", Plan: &p})
			continue
		}
		if prev, ok := approved[s.ID]; !ok || !prev.Same(p) {
			if err := approve(s.ID, p); err != nil {
				return res, err
			}
		}
		ui.Logf("%s: applying (+%d ~%d -%d)", s.ID, p.Add, p.Change, p.Destroy)
		st = ui.Begin(s.ID, "applying", p.Add+p.Change+p.Destroy)
		if err := r.ApplyPlan(ctx, st.Step); err != nil {
			st.End("failed", false)
			return res, ui.Wrap(ui.ExitTerraform, "fix the error above and re-run `bbctl apply "+d.Cfg.Name+"`; finished stacks are skipped", fmt.Errorf("%s: terraform apply: %w", s.ID, err))
		}
		st.End("applied", true)
		res = append(res, StackResult{ID: s.ID, Status: "applied", Plan: &p})
	}
	return res, nil
}

// Destroy tears stacks down in reverse order.
func (d *Deployment) Destroy(ctx context.Context, only map[string]bool, approve Approve) ([]StackResult, error) {
	var res []StackResult
	for i := len(d.Stacks) - 1; i >= 0; i-- {
		s := d.Stacks[i]
		if !selected(only, s.ID) {
			continue
		}
		exists, err := d.AWS.StateExists(ctx, d.Cfg.State.Bucket, s.StateKey)
		if err != nil {
			return res, ui.Wrap(ui.ExitAWS, "", err)
		}
		if !exists {
			res = append(res, StackResult{ID: s.ID, Status: "skipped", Reason: "never applied"})
			continue
		}
		ui.Logf("%s: planning destroy", s.ID)
		st := ui.Begin(s.ID, "planning", 0)
		r, err := d.Runner(ctx, s)
		if err != nil {
			st.End("", false)
			return res, err
		}
		p, err := r.Plan(ctx, tf.PlanDestroy)
		st.End("", false)
		if err != nil {
			return res, ui.Wrap(ui.ExitTerraform, "", fmt.Errorf("%s: terraform plan -destroy: %w", s.ID, err))
		}
		if p.Empty() {
			res = append(res, StackResult{ID: s.ID, Status: "unchanged", Reason: "nothing to destroy"})
			continue
		}
		if err := approve(s.ID, p); err != nil {
			return res, err
		}
		ui.Logf("%s: destroying %d resources", s.ID, p.Destroy)
		st = ui.Begin(s.ID, "destroying", p.Destroy)
		if err := r.ApplyPlan(ctx, st.Step); err != nil {
			st.End("failed", false)
			return res, ui.Wrap(ui.ExitTerraform, "fix the error above and re-run `bbctl destroy "+d.Cfg.Name+"`; finished stacks are skipped", fmt.Errorf("%s: terraform destroy: %w", s.ID, err))
		}
		st.End("destroyed", true)
		res = append(res, StackResult{ID: s.ID, Status: "destroyed", Plan: &p})
	}
	return res, nil
}

// StackStatus is a stack's outputs and, for node stacks, its instances.
type StackStatus struct {
	ID      string         `json:"id"`
	Applied bool           `json:"applied"`
	Outputs map[string]any `json:"outputs,omitempty"`
	Nodes   []awsx.Node    `json:"nodes,omitempty"`
}

func (d *Deployment) Status(ctx context.Context, step func()) ([]StackStatus, error) {
	var res []StackStatus
	for _, s := range d.Stacks {
		st := StackStatus{ID: s.ID}
		exists, err := d.AWS.StateExists(ctx, d.Cfg.State.Bucket, s.StateKey)
		if err != nil {
			return res, ui.Wrap(ui.ExitAWS, "", err)
		}
		if exists {
			out, err := d.Outputs(ctx, s)
			if err != nil {
				return res, err
			}
			st.Outputs = out
			st.Applied = len(out) > 0
			if ids := InstanceIDs(out); len(ids) > 0 {
				if st.Nodes, err = d.AWS.Nodes(ctx, ids); err != nil {
					return res, ui.Wrap(ui.ExitAWS, "", err)
				}
			}
		}
		res = append(res, st)
		if step != nil {
			step()
		}
	}
	return res, nil
}

// Outputs reads a stack's outputs from its state.
func (d *Deployment) Outputs(ctx context.Context, s render.Stack) (map[string]any, error) {
	r, err := d.Runner(ctx, s)
	if err != nil {
		return nil, err
	}
	out, err := r.Outputs(ctx)
	if err != nil {
		return nil, ui.Wrap(ui.ExitTerraform, "", fmt.Errorf("%s: terraform output: %w", s.ID, err))
	}
	return out, nil
}

// InstanceIDs extracts the instance_ids output of a node stack. The modules
// key it by node, "<pool>-<idx>" or "mgmt-<idx>"; the IDs come back in index
// order, which is what --node counts in. A plain list is taken as is.
func InstanceIDs(out map[string]any) []string {
	var ids []string
	switch raw := out["instance_ids"].(type) {
	case []any:
		for _, v := range raw {
			if s, ok := v.(string); ok {
				ids = append(ids, s)
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(raw))
		for k := range raw {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return nodeIndexLess(keys[i], keys[j]) })
		for _, k := range keys {
			if s, ok := raw[k].(string); ok {
				ids = append(ids, s)
			}
		}
	}
	return ids
}

// nodeIndexLess orders node keys by their numeric suffix, so pool1-10 comes
// after pool1-9; keys without one sort as strings.
func nodeIndexLess(a, b string) bool {
	ai, aok := nodeIndex(a)
	bi, bok := nodeIndex(b)
	if aok && bok && ai != bi {
		return ai < bi
	}
	return a < b
}

func nodeIndex(k string) (int, bool) {
	i := strings.LastIndexByte(k, '-')
	if i < 0 {
		return 0, false
	}
	n, err := strconv.Atoi(k[i+1:])
	return n, err == nil
}

// applied reports whether a stack has outputs in its state. Errors are
// returned, not treated as "not applied", so a permissions problem never
// makes a live stack look absent.
func (d *Deployment) applied(ctx context.Context, s render.Stack) (bool, error) {
	exists, err := d.AWS.StateExists(ctx, d.Cfg.State.Bucket, s.StateKey)
	if err != nil {
		return false, ui.Wrap(ui.ExitAWS, "", err)
	}
	if !exists {
		return false, nil
	}
	out, err := d.Outputs(ctx, s)
	if err != nil {
		return false, err
	}
	return len(out) > 0, nil
}

func keys(m map[string]bool) []string {
	var k []string
	for id := range m {
		k = append(k, id)
	}
	sort.Strings(k)
	return k
}

func selected(only map[string]bool, id string) bool {
	return len(only) == 0 || only[id]
}
