// Package deploy orchestrates a deployment: it resolves the toolchain,
// renders the stacks from bbctl.yaml and runs them in dependency order.
package deploy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"

	"github.com/migrx-io/backedblock/internal/awsx"
	"github.com/migrx-io/backedblock/internal/config"
	"github.com/migrx-io/backedblock/internal/render"
	"github.com/migrx-io/backedblock/internal/tf"
	"github.com/migrx-io/backedblock/internal/ui"
)

// WorkDirName holds generated stacks and keys, next to bbctl.yaml.
const WorkDirName = ".bbctl"

type Deployment struct {
	Cfg     *config.Config
	AWS     *awsx.Client
	Profile string
	WorkDir string
	Stacks  []render.Stack

	terraform  string
	modulesDir string
	modulesSHA string
	inputs     render.Inputs
	// noState is set by Prepare(BucketIfExists) when the state bucket does not
	// exist, i.e. nothing has been applied yet.
	noState bool
}

// BucketMode says what Prepare does about the Terraform state bucket.
type BucketMode int

const (
	// BucketRequire fails if the bucket is missing (commands that read state).
	BucketRequire BucketMode = iota
	// BucketIfExists tolerates its absence (plan, apply). Apply creates the
	// bucket itself, only after the first deploy is approved.
	BucketIfExists
)

// Open validates the config and connects to AWS. It does not touch Terraform.
func Open(ctx context.Context, cfg *config.Config, configPath, profile string) (*Deployment, error) {
	if errs := cfg.Validate(); len(errs) > 0 {
		e := ui.Errf(ui.ExitUsage, "fix "+configPath+" and re-run", "%s is invalid", configPath)
		e.Details = errs
		return nil, e
	}
	c, err := awsx.New(ctx, cfg.Region, profile)
	if err != nil {
		return nil, ui.Wrap(ui.ExitAWS, "configure AWS credentials (env vars, ~/.aws or `aws sso login`)", err)
	}
	// Absolute: terraform runs inside each stack dir, so relative paths
	// (plan files, module sources) would resolve against the wrong directory.
	work, err := filepath.Abs(filepath.Join(filepath.Dir(configPath), WorkDirName))
	if err != nil {
		return nil, err
	}
	return &Deployment{
		Cfg: cfg, AWS: c, Profile: profile, WorkDir: work,
		Stacks: render.Stacks(cfg, work),
	}, nil
}

// Prepare gets everything Terraform needs: the state bucket check (per
// mode), the terraform binary, the modules, an SSH key, and the rendered
// stacks. It never changes anything in AWS. Every step is idempotent.
func (d *Deployment) Prepare(ctx context.Context, mode BucketMode) error {
	exists, err := d.AWS.StateBucketExists(ctx, d.Cfg.State.Bucket)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "", err)
	}
	if !exists && mode == BucketRequire {
		return ui.Errf(ui.ExitAWS, "nothing is deployed yet; run `bbctl apply`", "state bucket s3://%s does not exist", d.Cfg.State.Bucket)
	}
	d.noState = !exists

	if d.terraform, err = tf.EnsureTerraform(ctx, d.Cfg.Terraform.Path, d.Cfg.Terraform.Version); err != nil {
		return ui.Wrap(ui.ExitError, "set terraform.path in bbctl.yaml to use an existing binary", err)
	}
	return d.Render(ctx)
}

// Render fetches the modules and writes the stacks, without touching AWS.
func (d *Deployment) Render(ctx context.Context) error {
	var err error
	if d.modulesDir, d.modulesSHA, err = tf.EnsureModules(ctx, d.Cfg.Modules.Ref, d.Cfg.Modules.Path); err != nil {
		return ui.Wrap(ui.ExitError, "check network access to github.com, or set modules.path to a local checkout", err)
	}
	pub, err := d.sshPublicKey()
	if err != nil {
		return err
	}
	d.inputs = render.Inputs{ModulesDir: d.modulesDir, SSHPublicKeyPath: pub}
	return render.Write(d.Cfg, d.Stacks, d.inputs)
}

// CheckSecrets fails unless the node secret exists and is complete.
func (d *Deployment) CheckSecrets(ctx context.Context) error {
	s, found, err := d.AWS.GetSecrets(ctx, d.Cfg.SecretsSSMPath)
	if err != nil {
		return ui.Wrap(ui.ExitAWS, "", fmt.Errorf("read %s: %w", d.Cfg.SecretsSSMPath, err))
	}
	if !found {
		return ui.Errf(ui.ExitAWS, "run `bbctl secrets generate`", "node secret %s does not exist", d.Cfg.SecretsSSMPath)
	}
	if miss := awsx.MissingSecrets(s); len(miss) > 0 {
		return ui.Errf(ui.ExitAWS, "run `bbctl secrets generate --force` or set them with `bbctl secrets set`", "node secret %s is missing %v", d.Cfg.SecretsSSMPath, miss)
	}
	return nil
}

func (d *Deployment) ModulesSHA() string { return d.modulesSHA }

// Runner returns an initialized Terraform runner for a stack.
func (d *Deployment) Runner(ctx context.Context, s render.Stack) (*tf.Runner, error) {
	return d.runnerIn(ctx, s, s.Dir)
}

func (d *Deployment) runnerIn(ctx context.Context, s render.Stack, dir string) (*tf.Runner, error) {
	r, err := tf.NewRunner(d.terraform, dir)
	if err != nil {
		return nil, err
	}
	if err := r.Init(ctx); err != nil {
		return nil, ui.Wrap(ui.ExitTerraform, "", fmt.Errorf("%s: terraform init: %w", s.ID, err))
	}
	return r, nil
}

// sshPublicKey returns the configured key, or generates a deployment key in
// the work dir. The network module always registers an EC2 key pair; with SSM
// access it is only used if someone later enables SSH.
func (d *Deployment) sshPublicKey() (string, error) {
	if p := d.Cfg.Network.SSHPublicKeyPath; p != "" {
		return p, nil
	}
	dir := filepath.Join(d.WorkDir, "ssh")
	priv, pub := filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "id_ed25519.pub")
	if _, err := os.Stat(pub); err == nil {
		return filepath.Abs(pub)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(sk, "bbctl "+d.Cfg.Name)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(priv, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	sp, err := ssh.NewPublicKey(pk)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(pub, ssh.MarshalAuthorizedKey(sp), 0o644); err != nil {
		return "", err
	}
	ui.Logf("generated SSH key %s", priv)
	return filepath.Abs(pub)
}
