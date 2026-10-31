# [backedblock.io](https://backedblock.io)

**Block storage for Kubernetes that keeps its capacity in object storage.** A
volume's blocks live in S3; its working set is served from a write-back cache on
local disks; pods attach over NVMe-oF/TCP and see an ordinary block device.

```
                        ╔═ backedblock.io ═════════════════════════╗
╔════════════╗          ║                                          ║░
║            ║░ NVMe-oF ║  ┏━━━━━━━━━━━━━━━┓     ┏━━━━━━━━━━━━━━┓  ║░
║    pod     ║░────────▶║  ┃   EBS cache   ┃────▶┃    Object    ┃  ║░
║            ║░◀────────║  ┃  (write-back) ┃◀────┃ Storage (S3) ┃  ║░
╚════════════╝░         ║  ┗━━━━━━━━━━━━━━━┛     ┗━━━━━━━━━━━━━━┛  ║░
 ░░░░░░░░░░░░░░         ║                                          ║░
                        ╚══════════════════════════════════════════╝░
                         ░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░
```

Most workloads never need all of a disk at once. Provisioned block storage
prices every gigabyte at the fast tier's rate anyway, hot or untouched for
months. backedblock.io splits the two: capacity comes from the bucket and is
thin-provisioned, the local disks only have to hold the working set, and what a
pod gets is `/dev/nvmeXnY` with a filesystem on it. Nothing to port, no
object-storage semantics leaking into the application.

**This repository is how you deploy it:** `bbctl`, a single-binary CLI that
creates the whole storage cluster in your AWS account from one config file. It
drives the Terraform modules in
[`migrx-io/terraform-aws-mgx`](https://github.com/migrx-io/terraform-aws-mgx).
Full docs at **[backedblock.io/docs](https://backedblock.io/docs)**.

---

## How does it work

Two planes, on opposite sides of the Kubernetes boundary:

```
   Kubernetes cluster                          storage cluster
  ┌─────────────────────────┐                 ┌─────────────────────────┐
  │  CSI Driver             │──── HTTP/s ────▶│  management API         │ ┐ control plane
  │                         │                 │                         │ ┘
  ├─────────────────────────┤                 ├─────────────────────────┤
  │  pod                    │                 │  NVMe target (SPDK)     │ ┐
  │   └─ /dev/nvmeXnY       │◀─ NVMe-oF/TCP ─▶│  nbdkit plugin          │ │ data plane
  │                         │                 │  cache on local disks   │ │
  │                         │                 │            │            │ ┘
  └─────────────────────────┘                 └────────────┼────────────┘
                                                           ▼ 1 MiB objects
                                                       ┌───────┐
                                                       │  S3   │
                                                       └───────┘
```

- **Control plane** — the CSI driver in Kubernetes talking HTTP/S to the storage
  cluster's management API. It creates, attaches, expands and snapshots volumes.
  A PVC becomes an API call, and what comes back is the address of a target to
  connect.
- **Data plane** — the volume itself: an NVMe-oF/TCP target on the storage
  nodes, a write-back cache on their local disks, and 1 MiB objects in S3. This
  is the path a pod's reads and writes actually take.

Details — the components on each plane, the path a block takes, cache
sizing: **[backedblock.io/docs/architecture](https://backedblock.io/docs/architecture)**.

---

## How is it deployed

The storage cluster runs on EC2, outside Kubernetes, in **your** AWS account and
an **existing VPC** — normally the same VPC your workloads already run in, so the
CSI driver reaches it over private addresses with nothing to open.

Cloud resources created in your AWS account for the storage cluster:

```
 ┌─ AWS account ──────────────────────────────────────────────────────────────┐
 │                                                                            │
 │ ┌─ your existing VPC ────────────────────────────────────────────────────┐ │
 │ │                                                                        │ │
 │ │    ┌──────────┐    ┌──────────┐    ┌──────────┐         ┌────────────┐ │ │
 │ │    │ ec2      │    │ ec2      │    │ ec2      │         │ bastion    │ │ │
 │ │    │ node 1   │    │ node 2   │    │ node 3   │         │ (optional) │ │ │
 │ │    └┬─┬────┬──┘    └┬─┬────┬──┘    └┬─┬────┬──┘         └────────────┘ │ │
 │ │     │ │    │        │ │    │        │ │    │            ssh jump host  │ │
 │ │     │ │ ┌──┴────┐   │ │ ┌──┴────┐   │ │ ┌──┴────┐       public subnet  │ │
 │ │     │ │ │  EBS  │   │ │ │  EBS  │   │ │ │  EBS  │                      │ │
 │ │     │ │ │ cache │   │ │ │ cache │   │ │ │ cache │       EBS cache      │ │
 │ │     │ │ └───────┘   │ │ └───────┘   │ │ └───────┘       disks per node │ │
 │ │     │ │             │ │             │ │                                │ │
 │ │ ────┴─┼─────────────┴─┼─────────────┴─┼─ mgmt subnet · HTTP/s API      │ │
 │ │       │               │               │                                │ │
 │ │ ──────┴───────────────┴───────────────┴─ data subnet · NVMe-oF/TCP     │ │
 │ └────────────────────────────────────┬───────────────────────────────────┘ │
 │                                      │ S3 gateway endpoint                 │
 │                                      ▼                                     │
 │            ┌─ S3 ─────────────────────────────────────────────┐            │
 │            │  ┌──────────────────┐    ┌────────────────────┐  │            │
 │            │  │ data buckets     │    │ backup buckets     │  │            │
 │            │  └──────────────────┘    └────────────────────┘  │            │
 │            └──────────────────────────────────────────────────┘            │
```

The infrastructure is the same shape for both planes — same AMI, same subnets,
same provisioning. The difference is that **control-plane nodes carry no cache
disks and need no buckets**: the data-path components do not run there, so there
is no I/O to cache and nothing to flush to S3. What is drawn above is a
data-plane pool; a management plane is the same picture without the EBS boxes and
the S3 group.

> **Note.** Small installations do not need separate control-plane nodes. The
> control-plane and data-plane components can run together on the same machines,
> which collapses the whole deployment into N equivalent nodes forming one
> storage pool — every node identical, every node running both planes. Larger
> deployments split them, with a management plane of its own and pools beneath
> it.

Those EC2 nodes, their cache disks and their buckets are one **pool** — the unit
volumes are placed in and served from. bbctl creates, through Terraform:

- **Two subnets per AZ, one network each way.** The *mgmt* subnet carries control
  traffic — the management API on `8082`, optional SSH, and metrics — and the
  *data* subnet carries volume I/O over NVMe-oF/TCP. Every node has an interface
  in both.
- **EC2 storage nodes**, each with its own **cache disks** — EBS volumes striped
  RAID0, or local NVMe. That cache is what gives a volume its latency, so its
  size is the number worth getting right before you apply.
- **Data and backup buckets** — the data buckets hold every block as a 1 MiB
  object, the backup buckets hold snapshots. Both are reached through an **S3
  gateway endpoint**, so block traffic never leaves the AWS network.
- A NAT gateway, a security group, and an **optional bastion** in a public subnet
  you provide — the ssh jump host for nodes that have no public IP.

---

## What do the nodes run

Every node boots a **prebaked AMI** with all the applications a storage cluster
needs already compiled and installed. Provisioning installs no software: it delivers
the per-node inputs (secrets, peer addresses, the pool registry) and runs the
baked `setup-node.sh` in place.

**One image, both planes.** The AMI is shared across the whole deployment, so
provisioning is mostly a choice of role — `mgmt` for a control-plane node, the
pool role for a data-plane node, or **both** on the same machine for simple
single-pool setups.

Base OS, architecture and the published AMI ID for each region:
[Node AMIs](https://backedblock.io/docs/node-amis).

The same stack runs on both roles; only the plugin set differs.

```
   control-plane node · role mgmt             data-plane node · role pool
  ┌────────────────────────────────┐         ┌────────────────────────────────┐
  │ HTTP/S API · JSON RPC :8082    │────────▶│ HTTP/S API · JSON RPC          │
  ├────────────────────────────────┤mgmt cmds├────────────────────────────────┤
  │ core                           │         │ core                           │
  │   joins nodes · elects leader  │         │   joins nodes · elects leader  │
  │   keeps the cluster running    │         │   keeps the cluster running    │
  ├────────────────────────────────┤         ├────────────────────────────────┤
  │ plugins                        │         │ plugins                        │
  │   management operations API    │         │   volume lifecycle · QoS       │
  │   pool registry · federation   │         │   NVMe target · cache · S3     │
  ├────────────────────────────────┤         ├────────────────────────────────┤
  │ cassandra · cluster metadata   │         │ cassandra · cluster metadata   │
  ├────────────────────────────────┤         ├────────────────────────────────┤
  │ prometheus (optional)          │         │ prometheus (optional)          │
  └────────────────────────────────┘         └────────────────────────────────┘
```

### Applications

- **core** — the distributed application every node runs. It joins nodes into a
  cluster, elects a leader, self-heals and keeps the cluster up. Each group of
  nodes is a self-sufficient pool with its own membership and its own leader, and
  does not depend on any other pool.
- **plugins** — core extensions, each responsible for one piece of
  functionality. Depending on the node's role core spawns a different set of
  them: management operations and the pool registry on a control-plane node,
  volume lifecycle, QoS and the data path on a data-plane node. That set is the
  only difference between the two roles.
- **Cassandra** — the persistence layer for cluster metadata.
- **HTTP/S API** — core exposes JSON RPC covering every cluster operation, the
  same surface for every caller: the CSI driver, your own automation, or a person
  at a terminal. It is also how a control-plane pool propagates management
  commands down to the data-plane pools.

### Metrics (optional)

A node can be started with the metrics feature enabled (`enable_metrics`), which
brings up Prometheus on the node to collect and aggregate metrics from core and
its plugins. `enable_grafana` adds Grafana on the cluster's VIP node.

---

## Install

`bbctl` is a single binary that deploys the whole storage cluster. It downloads
the Terraform version and modules it needs, talks to AWS through the SDK, and
reaches nodes over SSM with Session Manager built in, so you don't need
Terraform, Terragrunt, the AWS CLI, git or the Session Manager plugin. **You
only need AWS credentials.**

```bash
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/migrx-io/backedblock/main/install.sh | sh

# Windows: download bbctl_windows_amd64.zip (or _arm64) from
# https://github.com/migrx-io/backedblock/releases/latest and put bbctl.exe on your PATH

# Any OS with Go
go install github.com/migrx-io/backedblock/cmd/bbctl@latest
```

```bash
bbctl init                        # download Terraform and the modules
bbctl config create prod          # region, VPC, workload → prod.bbctl.yaml
bbctl secrets generate            # random node secrets → SSM SecureString
bbctl doctor                      # check everything apply needs
bbctl apply                       # state bucket, network, pools, mgmt, in order
bbctl status --wait               # until every node has finished provisioning
```

`config create` asks for the region, the VPC and the workload (volume count,
size, throughput, IOPS). It sends the workload to the
[cost calculator](https://backedblock.io/#calculator)'s API, which sizes the
pools: instance type, node count, cache disks and cache sizes. It also prints
the estimate and the StorageClass parameters. Every question has a flag (see
`bbctl config create --help`). A flag that is given is not asked, and without
a terminal nothing is asked.

Each storage is one file, `NAME.bbctl.yaml`, and one deployment. Several can
share a directory: commands pick one with `--storage NAME` (`-s`), or use the
only one there. `bbctl config list` shows them.

A workload too big for one pool gets several identical pools. Add more with
`bbctl pool add pool2`, then `bbctl apply`:
- **Without a management plane**, each pool is independent, with its own API
  endpoints and Grafana.
- **With one**, it presents one API for every pool and federates their
  metrics. `config create` asks whether to add it when there are several
  pools (`--mgmt` or `--mgmt=false` answers ahead), and `pool add --with-mgmt`
  adds it later.

Day-2 commands:

| Command | What it does |
|---|---|
| `bbctl exec [--all] -- CMD` | run a shell command on nodes via SSM Run Command and return its output and exit code; no SSH and no terminal |
| `bbctl connect [--forward 3000]` | interactive shell or port forward (Grafana) via Session Manager, built in; no plugin to install |
| `bbctl secrets get MGX_GW_ADMIN_PASSWD` | the `mgx-cli` admin password |
| `bbctl plan` / `bbctl destroy` | preview, or tear down in reverse order |
| `bbctl render` | write the generated Terraform to `.bbctl/NAME/stacks/` for review |

**Scripts and agents:** every command takes `--json`. The result goes to
stdout, progress and Terraform logs go to stderr, and failures come back as
`{"error", "hint", "code"}`. Exit codes: 2 config, 3 AWS, 4 Terraform, 5 declined.
Prompts are never shown without a TTY. `apply` and `destroy` need
`--auto-approve` instead. Every command is safe to re-run. See
[`AGENTS.md`](AGENTS.md).

### Managing the cluster with mgx-cli

Every node ships `mgx-cli`, and the nodes of a pool are equivalent members of
one cluster, so any node can drive the whole thing:

```
$ bbctl secrets get MGX_GW_ADMIN_PASSWD    # the login password
$ bbctl connect                            # shell on a node over Session Manager
$ mgx-cli
mgx-core:127.0.0.1:nologin> login admin --cluster main --ns main
Password:
mgx-core:127.0.0.1:main:main:admin> cluster nodes list
```

Every command set (nodes, pools, volumes, snapshots) is documented in the
[CLI reference](https://backedblock.io/docs/cli).

**Grafana** (when `enable_grafana` is on) runs on the cluster's VIP node, and the
VIP can move. Run `cluster list` in mgx-cli. `vip_host.ip` is the node that
currently holds it. Match that IP against `bbctl status`, then forward the port:

```bash
bbctl connect --node i-0123456789abcdef0 --forward 3000   # http://localhost:3000
```

### Requirements

- AWS credentials with EC2, IAM, VPC, S3, and SSM permissions
- An existing VPC with a public subnet for the NAT gateway. `bbctl config
  create` finds both.
- Access to the prebaked node AMI in your region. `bbctl config create` picks
  the one listed for your region in [Node AMIs](https://backedblock.io/docs/node-amis).

To call the modules from your own Terraform instead, see
[`terraform-aws-mgx`](https://github.com/migrx-io/terraform-aws-mgx). `bbctl render`
writes the exact stacks bbctl runs, if you want a working starting point.

---

## Related repositories

| Repository | What it is |
|---|---|
| [`terraform-aws-mgx`](https://github.com/migrx-io/terraform-aws-mgx) | The Terraform modules bbctl runs (`network`, `pool`, `mgmt`, `provision`), fetched over HTTPS at `modules.ref`. Usable directly from your own Terraform. |
| [`mgx-csi-driver`](https://github.com/migrx-io/mgx-csi-driver) | The CSI driver and its Helm chart: the Kubernetes half of the install. |

Questions: [support@backedblock.io](mailto:support@backedblock.io).
