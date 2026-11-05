# Deploying backedblock.io with an agent

Use `bbctl` and nothing else. You don't need to install Terraform, Terragrunt,
the AWS CLI or git, or to edit HCL. bbctl downloads its own pinned Terraform
and modules. The one prerequisite is AWS credentials in the standard chain
(env vars, `~/.aws`, SSO, instance role). If they're missing, stop and ask
the human.

## Contract

- Pass `--json` to every command. Stdout is exactly one JSON document. With
  `-v`, progress and Terraform logs go to stderr; without it there are none.
- Failure: `{"code": N, "error": "...", "hint": "...", "details": [...]}`. Do
  what `hint` says.
- Exit codes: `0` ok, `1` other, `2` bad flags or invalid config, `3`
  AWS (credentials, permissions, missing resources), `4` Terraform, `5`
  declined.
- Prompts never appear without a TTY. `apply` and `destroy` fail with code 2
  unless you pass `--auto-approve`.
- Every command is safe to re-run. After a failure, fix the cause and run the
  same command again; finished stacks are skipped.

## Deploy

```bash
bbctl init --json                            # downloads Terraform and the modules
bbctl config create <name> --region <region> --volumes <n> --volume-size <gb> --json
                                             # writes <name>.bbctl.yaml; add --vpc if the region has several VPCs
bbctl plan <name> --json                     # show the human the plan
bbctl apply <name> --auto-approve --json     # 10-20 min; creates the node secret if missing
bbctl status <name> --wait --json            # until every node reports provision "Success"
```

`config create` asks nothing under `--json`. Give the workload as flags:
`--volumes`, `--volume-size` (GB), `--volume-throughput` (MB/s) and
`--volume-iops`. Missing ones take the calculator's defaults. The calculator
sizes the pools. Its answer is in the result's `estimate`, with the monthly
cost against EBS gp3, and `storage_class` holds the StorageClass parameters
for the CSI driver. When the workload needs several pools, a management plane
is added in front of them unless you pass `--mgmt=false`. Ask the human which
they want.

The storage name is the first argument of every command that uses a config,
e.g. `bbctl pool add <name> pool2` or `bbctl exec <name> -- uptime`, and it is
required. `bbctl config list --json` lists them.
`bbctl config delete <name> --yes --json` removes the file only; it destroys
nothing in AWS, so run `destroy` first (with the human's approval) if the
storage is deployed.

Before `apply`, get the human's approval. It creates billable EC2, EBS, NAT
and S3 resources. Show them the `plan` output, the instance count and type
from `<name>.bbctl.yaml`, and the estimate from `config create`.

## Change

Edit `<name>.bbctl.yaml`, then run `plan` and `apply`. Module variables that have
no dedicated field go under a pool's `extra:` map, for example
`cache_filesystem: xfs` or `scheduler_node_down_wait: 120`.

- Add a pool: `bbctl pool add <name> <pool> [--az <az>]`, then `apply`. Without a
  management plane each pool is independent, with its own API endpoints and
  Grafana. `--with-mgmt` adds a management plane in front of all pools; after
  that apply, the CSI driver points at the mgmt nodes.
- Remove a pool: `bbctl pool remove <name> <pool> --auto-approve`. This **deletes its buckets
  and data**.
- Limit to one stack: `--stack network|pool/<pool>|mgmt`.

## Operate

```bash
bbctl exec <name> --json -- '<shell command>'            # one node, as root, via SSM Run Command
bbctl exec <name> --all --json -- '<shell command>'      # every node of the first pool
bbctl exec <name> --pool pool2 --node 1 --json -- '...'  # a specific node
bbctl secrets get <name> MGX_GW_ADMIN_PASSWD --json      # password for mgx-cli `login admin`
```

For the CSI driver, `bbctl sc <name> --json` gives the helm install as
`command`, with the API nodes (the management plane's, or without one the
first pool's), the cluster, the admin login and the StorageClass (cache sizes,
QoS caps) from the first pool's `storage_class:` in `<name>.bbctl.yaml`; QoS
caps it leaves out are 125 MB/s and 3000 IOPS. The password is left as
`bbctl secrets get <name> MGX_GW_ADMIN_PASSWD`. Installing changes the human's
Kubernetes cluster: show them the command.

Use `exec`, not `connect` or `cli`. They open interactive sessions (a shell,
or `mgx-cli` logged in to the cluster), which need a human at a terminal. Each `exec` result carries `status`,
`exit_code`, `stdout` and `stderr`. SSM truncates each stream at 24 KB.

## Tear down

`bbctl destroy <name> --auto-approve --json` deletes everything, including volume
data in the pool buckets. Never run it without the human's explicit approval.
The state bucket and the node secret are kept; `--purge` deletes them too
(the bucket only if no other deployment's state is in it). `bbctl secrets
delete <name> --yes --json` deletes just the secret.
