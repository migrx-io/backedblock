# mgx-storage starter (Terragrunt)

A ready-to-edit **golden-path** deployment of [mgx-storage](https://migrx.io) on
AWS, using [Terragrunt](https://terragrunt.gruntwork.io/) over the
[`migrx-io/terraform-aws-mgx`](https://github.com/migrx-io/terraform-aws-mgx) modules.

It provisions:

- **1 management plane** (3 nodes)
- **2 storage pools**, `pool1` and `pool2`, each with an **EBS RAID0 cache**
  (`raid_level = 0`), explicitly cross-granted IAM access to each other's S3 buckets

```
.
├── common.hcl              # all shared values — edit this first
├── root.hcl                # backend + provider generation (included by every unit)
├── bootstrap/              # Terraform: creates the S3 state bucket (run FIRST)
├── scripts/
│   └── new-pool.sh         # scaffold a new pools/<name>/ unit
├── secrets.env.example
├── network/                # foundation: subnets, NAT, SG, key pair (bastion off)
├── mgmt/                   # management plane (auto-discovers all pools)
└── pools/
    ├── _pool.hcl           # shared pool defaults (DRY)
    ├── pool1/              # a whole pool = one terragrunt.hcl
    └── pool2/
```

Each unit is its own state. Adding `pool3` is a new folder + one `terragrunt
apply`; removing a pool is `terragrunt destroy` in its folder — the other
components are untouched. The modules are pulled straight from GitHub.

## Prerequisites

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.4 and
  [Terragrunt](https://terragrunt.gruntwork.io/docs/getting-started/install/)
- AWS credentials with permissions for EC2, IAM, VPC, S3, and SSM
- An existing VPC and a **public** subnet (for the NAT gateway)
- An **S3 bucket** for Terraform state — created by the `bootstrap/` stack (step 1)
- A prebaked **mgx AMI** (built by [mgx-packer](https://github.com/migrx-io)) for
  `nodes_ami` — provisioning runs the baked `setup-node.sh` in place
- Node secrets (see **Provisioning**)

## Step 1 — bootstrap (state bucket + secret)

Two pre-deploy prerequisites:

**1. The S3 state bucket** (Terraform). The `bootstrap/` stack creates and hardens
it (versioning, encryption, public-access block). Set `state_bucket`/`region` in
`bootstrap/terraform.tfvars` (matching `common.hcl`), then:

```bash
cd bootstrap
terraform init
terraform apply        # creates the S3 state bucket
cd ..
```

`bootstrap` uses **local** state. State locking for the other stacks is S3-native —
uncomment `use_lockfile = true` in `root.hcl` to enable it.

**2. The node secrets** (AWS CLI — *not* Terraform, so the value never lands in any
state). Create them in the SSM SecureString that `secrets_ssm_path` points at
(`/mgx/<cluster>/secrets`):

```bash
cp secrets.env.example secrets.env   # then edit with real values (git-ignored)
aws ssm put-parameter --type SecureString \
  --name /mgx/main/secrets --value file://secrets.env
```

## Step 2 — configure

Edit **`common.hcl`** — `state_bucket` and `cluster` (matching step 1), `vpc_id`,
subnet CIDRs, `bastion.vpc_subnet` (used for the NAT gateway), `nodes_ami`, and
`key_name`.

> `azs` in `common.hcl` is the set of AZs the **network** builds subnets in. Each
> pool pins itself to a **single** AZ via its own `az` in
> `pools/<pool>/terragrunt.hcl` (EBS RAID0 cache volumes are AZ-bound) — that `az`
> must be one of `azs`. So you can spread pools across AZs (pool1→`us-east-1a`,
> pool2→`us-east-1b`, …). Shared pool sizing lives in `pools/_pool.hcl`; per-pool
> identity, AZ, buckets, and cross-grant live in each pool's `terragrunt.hcl`.

Pin `modules_ref` to a released tag when one is available (defaults to `main`).

**Provisioning is SSM here** (agentless, no bastion). setup-node.sh is run
via SSM Run Command; nodes fetch the secrets from the SecureString created in
step 1 and reach the SSM endpoints through the NAT gateway. (The `terraform-aws-mgx`
modules also support `provision_mode = "ssh"`; this starter standardizes on `ssm`,
so `bastion.enable` is `false`.)

## Step 3 — deploy (Terragrunt)

Bring up everything in dependency order (network → pools → mgmt):

```bash
terragrunt run --all -- apply     # Terragrunt v0.x: terragrunt run-all apply
```

Or one component at a time:

```bash
(cd network     && terragrunt apply)
(cd pools/pool1 && terragrunt apply)
(cd pools/pool2 && terragrunt apply)
(cd mgmt        && terragrunt apply)
```

Apply `mgmt` **after** the pools. Each pool's apply writes a small registry entry
to SSM (`/mgx/<cluster>/pools/<pool>` — an `aws_ssm_parameter`). `mgmt`'s apply
reads them back with an `aws_ssm_parameters_by_path` **data source** — discovery
happens at apply time, by whoever runs Terragrunt (using your AWS credentials), so
the pools' parameters must already exist. `mgmt` then renders the pool list into
`pool_info.json` and delivers it to the mgmt nodes via SSM Run Command. **Re-run
`mgmt` whenever you add or remove a pool** to pick up the change; `run --all`
handles the ordering on a full apply.

## Day-2

| Task | Action |
|------|--------|
| Add a pool | `scripts/new-pool.sh pool3`, (optional) edit its `s3_bucket_access_names`, `(cd pools/pool3 && terragrunt apply)`, then re-apply `mgmt`. No `mgmt` edit needed — it auto-discovers pools. |
| Remove a pool | `(cd pools/pool3 && terragrunt destroy)`, delete `pools/pool3/`, then re-apply `mgmt`. |
| Scale a pool | Change `nodes_count` in the pool's `terragrunt.hcl`, `(cd pools/<pool> && terragrunt apply)`. |
| Tear down all | `terragrunt run --all -- destroy`. |

## Scaling to many pools

Each pool is a single ~25-line `terragrunt.hcl`, state is isolated per pool, and
`run --all` builds the dependency graph and applies in parallel — the right shape
for large fleets. Generate pools with `scripts/new-pool.sh`:

```bash
scripts/new-pool.sh pool3                                  # az us-east-1a, buckets from name
scripts/new-pool.sh pool4 us-east-1b                       # pin to a specific AZ
scripts/new-pool.sh pool5 us-east-1c data-bkt backup-bkt   # AZ + explicit buckets
for i in $(seq 6 100); do scripts/new-pool.sh "pool$i"; done   # bulk
```

`mgmt` auto-discovers every `pools/*/` unit (via `fileset` in `mgmt/terragrunt.hcl`),
so adding pools needs no edit there. Cross-grant is explicit per pool, so the IAM
policy stays small no matter how many pools exist.

## Notes

- `backend.tf` and `provider.tf` are generated per unit by `root.hcl` and are
  git-ignored.
- Apply the **network first** — `mgmt`/pools read its outputs from state, so a
  `plan`/`apply` on them before the network exists will fail. `run --all` and the
  ordered single-unit commands above handle this for you.
- `nodes_ami` and `bastion.ami` must be valid for your `region`.
