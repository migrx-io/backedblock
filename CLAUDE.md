# CLAUDE.md

This repo is `bbctl`, a single Go binary that deploys a backedblock.io storage
cluster into a user's AWS account from one `bbctl.yaml`. It downloads a pinned
Terraform and the `migrx-io/terraform-aws-mgx` modules (as an HTTPS tarball,
no git), talks to AWS through the SDK, and reaches nodes over SSM. The goal is
that **AWS credentials are the only prerequisite**, for humans and for agents.

How the tool is used (the agent-facing contract): @AGENTS.md

## Layout

```
cmd/bbctl/main.go       entry point; maps errors to exit codes
internal/cli/           cobra commands (one file per area: init, doctor, secrets, deploy, nodes, pool)
internal/config/        bbctl.yaml schema, Validate(), defaults, atomic Save/Stage
internal/render/        config -> one Terraform root per state (main.tf.json), WriteLocal for previews
internal/deploy/        orchestration: Prepare, Plan, Apply, Destroy, Status
internal/tf/            terraform install (hc-install), module fetch, tfexec runner
internal/awsx/          SDK calls: discovery, state bucket, secrets, SSM exec/session
internal/ui/            output contract: Result, Errf/Wrap, Confirm, exit codes
```

Stacks, in apply order: `network`, `pool/<name>`..., `mgmt` (mgmt discovers
pools from SSM, so it comes last). Each stack has its own state key in the
S3 state bucket and uses S3-native locking (`use_lockfile`, needs Terraform
1.10 or later). Pools and mgmt read network outputs via `terraform_remote_state`.
The generated stacks live in `.bbctl/stacks/` next to `bbctl.yaml` (git-ignored).

## Rules the code depends on

Each of these fixed a real bug or review finding. Keep them.

- **Stdout carries exactly one result:** JSON with `--json`, else text.
  Progress (`ui.Logf`) and Terraform's output go to stderr. Errors are
  `ui.Error{code, error, hint, details}`.
- **Exit codes are stable:** 1 other, 2 usage or config, 3 AWS, 4 Terraform,
  5 declined. `cli.SilentExit` sets a code after a result was already
  printed. `main` matches that concrete type only, because wrapped
  `*exec.ExitError`s also have `ExitCode()`.
- **Nothing is created before approval.**
  - `Prepare` never writes to AWS.
  - On a first deploy, `apply` previews the network against a local state
    (`render.WriteLocal`) and lists the state bucket in the approval. It
    creates the bucket only after that approval, and skips re-prompting if
    the real plan is `Same()` as the approved one.
- **`plan` is read-only:** it never creates the bucket and runs with
  `-lock=false` (`tf.PlanReadOnly`).
- **Only a 404 means "doesn't exist".** `StateBucketExists` and `StateExists`
  return errors for 403, 301 and anything else. Treating them as missing
  once made `destroy` skip live stacks and `plan` preview a live deployment
  as new.
- **Prompts never block without a TTY.** `ui.Confirm` fails with the flag to
  pass (`--auto-approve`).
- **The work dir is absolute.** Terraform runs inside each stack dir, so
  relative plan paths break. Module sources are written as `./` or `../`
  paths relative to the stack dir; Terraform treats nothing else as local.
- **Config writes are atomic** (temp file plus rename). Before an
  irreversible step, `Stage()` the new config first and `Commit()` after;
  see `pool remove`.
- **`tfexec.SetEnv` replaces the whole environment.** Pass `os.Environ()`
  plus extras, minus `tfexec.ProhibitedEnv`.
- **Commands are safe to re-run.** Errors carry a hint naming the next
  command.
- **The management plane is explicit.** It's only added by `init --mgmt` or
  `pool add --with-mgmt`, never automatically. Several pools without mgmt is
  a valid layout: independent pools, each with its own API and Grafana.
  `cross_peer_scrape` and the pools' Grafana follow whether `mgmt` exists.

## Develop

```bash
go build ./... && go vet ./... && go test ./...
gofmt -l internal cmd                                   # must print nothing
for t in linux/amd64 linux/arm64 darwin/arm64 windows/amd64; do \
  GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go build -o /dev/null ./cmd/bbctl; done
```

Unit tests cover pure logic: CIDR carving, route classification, the 404
handling, config validation and staging, and rendering. Anything that needs
AWS is smoke-tested by hand:

```bash
go build -o $SCRATCH/bbctl ./cmd/bbctl
cd $SCRATCH/smoke && $SCRATCH/bbctl init --region us-east-1 --name bbsmoke --force
$SCRATCH/bbctl doctor; $SCRATCH/bbctl plan --json; $SCRATCH/bbctl render
# validate rendered stacks without a backend:
TF=~/Library/Caches/bbctl/terraform/<ver>/terraform
(cd .bbctl/stacks/network && $TF init -backend=false && $TF validate)
```

`init`, `doctor`, `render` and `plan` are read-only in AWS. `apply`,
`destroy`, `secrets generate|set` and `pool remove` change the account and
cost money. **Ask the user before running them.** To exercise one of those
paths without creating anything, write a throwaway `_test.go` that swaps in
a declining approver or a failing S3 client, then delete it.

Useful env vars: `BBCTL_TERRAFORM` (use a given binary), `BBCTL_CACHE_DIR`,
`GITHUB_TOKEN` (raises the GitHub rate limit for module ref resolution). To
develop against local modules, set `modules.path` in `bbctl.yaml` to a
checkout such as `../terraform-aws-mgx`.

## Related repos (siblings under ~/source/migrx)

- `terraform-aws-mgx`: the modules bbctl renders against. When a module
  variable changes, update `render.go` and `config.go` together.
- `mgx-packer`: builds the node AMIs. Names are
  `mgx-storage-ubuntu<ver>-<arch>-<ts>`, owner `awsx.AMIOwner`.
- `backedblock.io`: the website and docs.
- `mgx-csi-driver`: the Kubernetes half of the install.

## Conventions

- Match the surrounding style: short doc comments that say why, and flat
  error handling with `ui.Wrap(code, hint, err)`.
- Every new command gets `--json` output through `ui.Result`, an error hint,
  and an entry in the README and `AGENTS.md` if users or agents call it.
- Commit messages are lowercase imperative, e.g. `add bbctl CLI, ...`.
