# backedblock.io

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

**This repository is how you deploy it.** Two ready-to-edit Terraform
blueprints for the storage cluster, over the modules in
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
volumes are placed in and served from. Terraform creates:

- **Two subnets per AZ, one network each way.** The *mgmt* subnet carries control
  traffic — the management API on `8082`, ssh, metrics — and the *data* subnet
  carries volume I/O over NVMe-oF/TCP. Every node has an interface in both.
- **EC2 storage nodes**, each with its own **cache disks** — EBS volumes striped
  RAID0, or local NVMe. That cache is what gives a volume its latency, so its
  size is the number worth getting right before you apply.
- **Two S3 buckets** — a *data* bucket holding every block as a 1 MiB object,
  and a *backup* bucket holding snapshots. Both are reached through an **S3
  gateway endpoint**, so block traffic never leaves the AWS network.
- A NAT gateway, a security group, and an **optional bastion** in a public subnet
  you provide — the ssh jump host for nodes that have no public IP.

Pools are independent — applying or destroying one leaves the others alone. A
fleet's **management plane** discovers every pool, presents one API for all of
them and federates their metrics; whether you run one or talk to the pool nodes
directly, the API is the same, so the CSI driver is configured the same.

Nodes boot from a **prebaked AMI** with every package and the node scripts
already in it. Terraform installs no software: it delivers the per-node inputs
(secrets, peer addresses, the pool registry) and runs the baked `setup-node.sh`
in place.

---

## Install

Two steps, in this order.

### 1. The storage cluster (this repo)

Pick the blueprint that matches your scale — they're independent, each in its own
directory.

|  | [`single-pool/`](single-pool) | [`terragrunt-scale/`](terragrunt-scale) |
|---|---|---|
| **Use when** | one pool, a lab, a PoC | many pools, a real fleet |
| **Tooling** | plain Terraform | [Terragrunt](https://terragrunt.gruntwork.io/) (DRY, `run --all`) |
| **Topology** | 1 storage pool | management plane + N storage pools |
| **Provisioning** | SSH via bastion | SSM Run Command (agentless) |
| **Secrets** | local `secrets.env`, uploaded over SSH | SSM SecureString |
| **Pool discovery** | n/a (single pool) | pools auto-discovered via SSM Parameter Store |
| **State** | local (per stack) | S3 backend (per unit) |
| **Bastion** | required (it's the provisioning path) | optional — only for SSH access to nodes |

- **Just want one pool running?** → [`single-pool/`](single-pool). Two
  `terraform apply`s (network, then pool), no Terragrunt, no SSM. Provisioning
  happens over SSH through a bastion.

- **Running a fleet?** → [`terragrunt-scale/`](terragrunt-scale). Each pool is a
  ~25-line `terragrunt.hcl`, `run --all` builds the dependency graph, a mgmt
  plane auto-discovers every pool, and provisioning is agentless over SSM (no
  bastion needed — though you can enable one purely for SSH access).

You'll need: Terraform >= 1.4, AWS credentials (EC2, IAM, VPC, S3, and SSM for
the fleet layout), an existing VPC with a public subnet for the NAT gateway, an
SSH key pair, and a prebaked node AMI for `nodes_ami` — ask
[hello@backedblock.io](mailto:hello@backedblock.io) for the current image ID in
your region.

See each directory's `README.md` for the full steps, and
[Install the storage cluster](https://backedblock.io/docs/storage-cluster) for
the same ground with the reasoning attached — including how to size the cache
before you apply.

### 2. The CSI driver (Kubernetes)

Once the cluster answers on `8082`, install
[`migrx-io/mgx-csi-driver`](https://github.com/migrx-io/mgx-csi-driver) into
Kubernetes:

```bash
helm install mgx-csi-driver oci://docker.io/migrx/mgx-csi-driver \
  --namespace mgx-system --create-namespace \
  --version 0.1.0 \
  --set csiSecret.clusterConfig.nodes='{10.0.1.10:8082,10.0.1.11:8082,10.0.1.12:8082}' \
  --set csiSecret.clusterConfig.username=admin \
  --set csiSecret.clusterConfig.password=<MGX_GW_ADMIN_PASSWD> \
  --set externalSnapshotter.enabled=true
```

`nodes` is every API endpoint of the storage cluster — the mgmt nodes' IPs if you
deployed a management plane, the pool's `node_mgmt_private_ips` if you didn't.
The password is `MGX_GW_ADMIN_PASSWD` from `secrets.env`. Full walkthrough:
[Quickstart](https://backedblock.io/docs/quickstart).

Then a volume is an ordinary PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: mgxcsi-sc
  resources:
    requests:
      storage: 100Gi
```

Thin-provisioned, expandable online, snapshottable to S3. `ReadWriteOnce` only —
this is block storage, not a shared filesystem.

---

## Layout

```
.
├── single-pool/        plain Terraform, one pool, SSH provisioning
│   ├── network/        foundation: subnets, NAT, SG, key pair, bastion
│   └── pool/           the storage pool
└── terragrunt-scale/   Terragrunt, mgmt plane + N pools, SSM provisioning
    ├── bootstrap/      creates the S3 state bucket (run first)
    ├── common.hcl      all shared values — edit this first
    ├── network/        foundation
    ├── mgmt/           management plane (auto-discovers pools)
    ├── pools/          one folder per pool
    └── scripts/        new-pool.sh — scaffold a pool
```

## Related repositories

| Repository | What it is |
|---|---|
| [`terraform-aws-mgx`](https://github.com/migrx-io/terraform-aws-mgx) | The Terraform modules these blueprints call (`network`, `pool`, `mgmt`, `provision`). Pulled straight from GitHub — nothing to vendor. |
| [`mgx-csi-driver`](https://github.com/migrx-io/mgx-csi-driver) | The CSI driver and its Helm chart: the Kubernetes half of the install. |

## Docs

- [Introduction](https://backedblock.io/docs) — what it is, and the tradeoffs
- [Install the storage cluster](https://backedblock.io/docs/storage-cluster)
- [Quickstart](https://backedblock.io/docs/quickstart) — driver, PVC, pod
- [Architecture](https://backedblock.io/docs/architecture)
- [Caching and tiering](https://backedblock.io/docs/caching)
- [Snapshots and restore](https://backedblock.io/docs/snapshots)
- [StorageClass parameters](https://backedblock.io/docs/storage-class)

Questions, or an AMI for your region: [hello@backedblock.io](mailto:hello@backedblock.io).
