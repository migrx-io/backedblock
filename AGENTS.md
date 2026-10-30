# Deploying backedblock.io with an agent

Use `bbctl` and nothing else. You don't need to install Terraform, Terragrunt,
the AWS CLI or git, or to edit HCL. bbctl downloads its own pinned Terraform
and modules. The one prerequisite is AWS credentials in the standard chain
(env vars, `~/.aws`, SSO, instance role). If they're missing, stop and ask
the human.

## Contract

- Pass `--json` to every command. Stdout is exactly one JSON document. Progress
  and Terraform logs go to stderr.
- Failure: `{"code": N, "error": "...", "hint": "...", "details": [...]}`. Do
  what `hint` says.
- Exit codes: `0` ok, `1` other, `2` bad flags or invalid `bbctl.yaml`, `3`
  AWS (credentials, permissions, missing resources), `4` Terraform, `5`
  declined.
- Prompts never appear without a TTY. `apply` and `destroy` fail with code 2
  unless you pass `--auto-approve`.
- Every command is safe to re-run. After a failure, fix the cause and run the
  same command again; finished stacks are skipped.

## Deploy

```bash
bbctl init --region <region> --json          # writes bbctl.yaml; add --vpc if the region has several VPCs
bbctl secrets generate --json                # no-op ("exists") if already there
bbctl doctor --json                          # every check must be "ok" or "warn"
bbctl plan --json                            # show the human the plan
bbctl apply --auto-approve --json            # 10-20 min
bbctl status --json                          # nodes: state "running", ssm "Online"
```

Before `apply`, get the human's approval. It creates billable EC2, EBS, NAT
and S3 resources. Show them the `plan` output and the instance count and type
from `bbctl.yaml`.

## Change

Edit `bbctl.yaml`, then run `plan` and `apply`. Module variables that have
no dedicated field go under a pool's `extra:` map, for example
`cache_filesystem: xfs` or `scheduler_node_down_wait: 120`.

- Add a pool: `bbctl pool add <name> [--az <az>] [--with-mgmt]`, then `apply`.
- Remove a pool: `bbctl pool remove <name> --auto-approve`. This **deletes its buckets
  and data**.
- Limit to one stack: `--stack network|pool/<name>|mgmt`.

## Operate

```bash
bbctl exec --json -- '<shell command>'            # one node, as root, via SSM Run Command
bbctl exec --all --json -- '<shell command>'      # every node of the first pool
bbctl exec --pool pool2 --node 1 --json -- '...'  # a specific node
bbctl secrets get MGX_GW_ADMIN_PASSWD --json      # password for mgx-cli `login admin`
```

Use `exec`, not `connect`. `connect` opens an interactive session, which
needs a human at a terminal. Each `exec` result carries `status`,
`exit_code`, `stdout` and `stderr`. SSM truncates each stream at 24 KB.

## Tear down

`bbctl destroy --auto-approve --json` deletes everything, including volume
data in the pool buckets. Never run it without the human's explicit approval.
The state bucket and the node secret are kept.
