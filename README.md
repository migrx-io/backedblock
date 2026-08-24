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

## How it fits together

Two planes, on opposite sides of the Kubernetes boundary. Kubernetes runs the
CSI driver and nothing else — no capacity, no cache, no part of the data path.
The storage cluster owns the disks, the buckets and the volumes, and is deployed
outside Kubernetes (that is what this repo does).

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

The **control plane** creates, attaches, expands and snapshots volumes; the
**data plane** carries blocks. They fail independently — a control plane that is
down cannot provision anything, but it does not stop a mounted volume from
serving I/O.

A write is acknowledged once it is in the cache and flushed to the bucket in the
background, coalesced into whole 1 MiB blocks. A read is served from the cache if
the block is resident, and otherwise fetches that one 1 MiB block from S3 first —
so cache sizing is the tuning knob that matters. See
[Architecture](https://backedblock.io/docs/architecture) and
[Caching](https://backedblock.io/docs/caching).

What Terraform builds, in three stacks:

```
   AWS account, one VPC
  ┌────────────────────────────────────────────────────────────────┐
  │  network stack    subnets per AZ · NAT · S3 VPC endpoint · SG  │
  │  (once)           SSH key pair · bastion (optional)            │
  ├────────────────────────────────────────────────────────────────┤
  │  pool stack       N storage nodes · cache disks · S3 buckets   │
  │  (one per pool)   NVMe-oF targets · node API :8081             │
  ├────────────────────────────────────────────────────────────────┤
  │  mgmt stack       M management nodes · gateway API :8082       │
  │  (fleets only)    pool registry · Prometheus · Grafana         │
  └────────────────────────────────────────────────────────────────┘
```

A **pool** is a set of storage nodes sharing a cache tier and a set of S3
buckets — where volumes are placed and served from. Pools are independent:
applying or destroying one leaves the others alone. The **management plane** is
optional; small installations talk to the pool nodes directly, a fleet gets a
mgmt stack that discovers every pool, presents one API for all of them and
federates their metrics. Either way the API is HTTP/S on port `8082`, so the CSI
driver is configured the same.

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
