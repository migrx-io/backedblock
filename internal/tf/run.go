package tf

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"

	"github.com/migrx-io/backedblock/internal/ui"
)

// Runner runs Terraform in one stack directory. With --verbose, Terraform's
// own output is streamed to stderr; stdout stays reserved for bbctl's result.
type Runner struct {
	tf  *tfexec.Terraform
	dir string
}

// NewRunner prepares Terraform for one stack. profile is bbctl's --profile;
// Terraform's provider and S3 backend must use the same AWS account as bbctl's
// own SDK calls, so it is passed on (see terraformEnv).
func NewRunner(execPath, dir, profile string) (*Runner, error) {
	t, err := tfexec.NewTerraform(dir, execPath)
	if err != nil {
		return nil, err
	}
	// tfexec puts Terraform's stderr in the error it returns, which bbctl
	// prints; streaming it too would show every error twice.
	if ui.Verbose {
		t.SetStdout(os.Stderr)
		t.SetStderr(os.Stderr)
	}

	// Share one provider cache across stacks and runs.
	pluginCache := ""
	if cache, err := CacheDir(); err == nil {
		pc := filepath.Join(cache, "plugins")
		if os.MkdirAll(pc, 0o755) == nil {
			pluginCache = pc
		}
	}
	if err := t.SetEnv(terraformEnv(os.Environ(), profile, pluginCache)); err != nil {
		return nil, err
	}
	return &Runner{tf: t, dir: dir}, nil
}

func (r *Runner) Init(ctx context.Context) error {
	return r.tf.Init(ctx, tfexec.Reconfigure(true))
}

// staticCredentialEnv are the variables that would let Terraform use access
// keys instead of the requested profile.
var staticCredentialEnv = []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"}

// terraformEnv builds Terraform's environment from bbctl's. tfexec.SetEnv
// replaces the whole environment, so it starts from environ.
//
// With a profile, Terraform gets AWS_PROFILE and no static keys, so it
// resolves the same credentials bbctl's SDK does for an explicit profile (the
// SDK ranks an explicit profile above environment keys). Without one,
// Terraform inherits the environment as-is, as bbctl's SDK does.
func terraformEnv(environ []string, profile, pluginCache string) map[string]string {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	for _, k := range tfexec.ProhibitedEnv(env) {
		delete(env, k)
	}
	if profile != "" {
		env["AWS_PROFILE"] = profile
		delete(env, "AWS_DEFAULT_PROFILE")
		for _, k := range staticCredentialEnv {
			delete(env, k)
		}
	}
	if pluginCache != "" {
		env["TF_PLUGIN_CACHE_DIR"] = pluginCache
	}
	return env
}

// Change is one resource in a plan.
type Change struct {
	Address string `json:"address"`
	Action  string `json:"action"` // create, update, delete, replace
}

// PlanSummary is a plan reduced to what a reviewer needs.
type PlanSummary struct {
	Add     int      `json:"add"`
	Change  int      `json:"change"`
	Destroy int      `json:"destroy"`
	Changes []Change `json:"changes,omitempty"`
}

func (p PlanSummary) Empty() bool { return p.Add+p.Change+p.Destroy == 0 }

// Same reports whether two plans make the same changes to the same resources.
func (p PlanSummary) Same(o PlanSummary) bool {
	if len(p.Changes) != len(o.Changes) {
		return false
	}
	for i := range p.Changes {
		if p.Changes[i] != o.Changes[i] {
			return false
		}
	}
	return true
}

func (r *Runner) planFile() string { return filepath.Join(r.dir, "bbctl.tfplan") }

// PlanMode selects what a plan is for.
type PlanMode int

const (
	// PlanApply plans changes that ApplyPlan will apply.
	PlanApply PlanMode = iota
	// PlanDestroy plans a destroy that ApplyPlan will apply.
	PlanDestroy
	// PlanReadOnly previews changes without taking the state lock, which
	// with S3-native locking would write a lock object to the bucket.
	PlanReadOnly
)

// Plan writes a plan file and summarizes it.
func (r *Runner) Plan(ctx context.Context, mode PlanMode) (PlanSummary, error) {
	opts := []tfexec.PlanOption{tfexec.Out(r.planFile())}
	switch mode {
	case PlanDestroy:
		opts = append(opts, tfexec.Destroy(true))
	case PlanReadOnly:
		opts = append(opts, tfexec.Lock(false))
	}
	if _, err := r.tf.Plan(ctx, opts...); err != nil {
		return PlanSummary{}, err
	}
	plan, err := r.tf.ShowPlanFile(ctx, r.planFile())
	if err != nil {
		return PlanSummary{}, err
	}
	return summarize(plan), nil
}

// ApplyPlan applies the plan file written by the last Plan. step, if not
// nil, is called as each resource finishes (a replace counts twice, as in the
// plan's totals). Without --verbose, Terraform's output is read only for that.
func (r *Runner) ApplyPlan(ctx context.Context, step func()) error {
	defer os.Remove(r.planFile())
	if step != nil && !ui.Verbose {
		r.tf.SetStdout(&finishedLines{step: step})
		defer r.tf.SetStdout(io.Discard)
	}
	return r.tf.Apply(ctx, tfexec.DirOrPlan(r.planFile()))
}

// finishedLines calls step for each "<addr>: Creation complete after 3s"
// style line of Terraform's output.
type finishedLines struct {
	step func()
	buf  []byte
}

var finished = []string{": Creation complete after", ": Modifications complete after", ": Destruction complete after"}

func (f *finishedLines) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(f.buf[:i])
		f.buf = f.buf[i+1:]
		for _, m := range finished {
			if strings.Contains(line, m) {
				f.step()
				break
			}
		}
	}
}

// Outputs returns the stack's outputs as plain JSON values. A stack that was
// never applied has none.
func (r *Runner) Outputs(ctx context.Context) (map[string]any, error) {
	raw, err := r.tf.Output(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]any, len(raw))
	for k, m := range raw {
		var v any
		if err := json.Unmarshal(m.Value, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, nil
}

func summarize(p *tfjson.Plan) PlanSummary {
	var s PlanSummary
	for _, rc := range p.ResourceChanges {
		if rc.Change == nil || rc.Mode != tfjson.ManagedResourceMode {
			continue
		}
		a := rc.Change.Actions
		var action string
		switch {
		case a.Replace():
			action = "replace"
			s.Add++
			s.Destroy++
		case a.Create():
			action = "create"
			s.Add++
		case a.Update():
			action = "update"
			s.Change++
		case a.Delete():
			action = "delete"
			s.Destroy++
		default:
			continue
		}
		s.Changes = append(s.Changes, Change{Address: rc.Address, Action: action})
	}
	return s
}
