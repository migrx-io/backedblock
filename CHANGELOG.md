# Changelog

## v0.1.0

The first release of `bbctl`, a single binary that deploys a backedblock.io
storage cluster into your AWS account. AWS credentials are the only
prerequisite: bbctl downloads its own pinned Terraform and the
`terraform-aws-mgx` modules.

- `init` downloads Terraform and the modules.
- `config create` writes `NAME.bbctl.yaml` from your VPC and a workload
  (volumes, size, throughput, IOPS), sized by the backedblock.io calculator,
  with an optional management plane in front of several pools.
- `plan`, `apply` and `destroy` run the network, pool and mgmt stacks, with
  state in an S3 bucket. Nothing is created before you approve the plan.
- `status` shows the stacks and each node's EC2, SSM and provisioning state.
- `pool add`, `pool list` and `pool remove` change the pools in a config.
- `secrets` manages the node secret in SSM Parameter Store.
- `exec`, `connect` and `cli` reach nodes over SSM, with Session Manager
  built in: run a command, open a shell or forward a port, or open
  `mgx-cli` logged in to the cluster.
- `sc` prints the helm install of the CSI driver and its StorageClass.
- `ui` opens a local dashboard of the pools: overview, hosts, cache,
  volumes and snapshots.
- Every command takes `--json` for agents, with stable exit codes and a hint
  on each error.

Binaries for Linux, macOS and Windows on amd64 and arm64.
