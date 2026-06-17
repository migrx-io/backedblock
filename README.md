# mgx-storage starter (Terragrunt)

A ready-to-edit **golden-path** deployment of [mgx-storage](https://migrx.io) on
AWS, using [Terragrunt](https://terragrunt.gruntwork.io/) over the
[`migrx-io/terraform-aws-mgx`](https://github.com/migrx-io/terraform-aws-mgx) modules.

It provisions:

- **1 management plane** (3 nodes)
- **2 storage pools**, `pool1` and `pool2`, each with an **EBS RAID0 cache**
  (`raid_level = 0`), cross-granted access to each other's S3 buckets

```
.
├── common.hcl              # all shared values — edit this first
├── terragrunt.hcl          # backend + provider generation (root)
├── network/                # foundation: subnets, NAT, SG, bastion, key pair
├── mgmt/                   # management plane (depends on network + pools)
└── pools/
    ├── _pool.hcl           # shared pool defaults (DRY)
    ├── pool1/
    └── pool2/
```

Each directory is an independent state. Adding `pool3` is a new folder + one
`terragrunt apply`; removing a pool is `terragrunt destroy` in its folder. The
other components are untouched.

## Prerequisites

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.4 and
  [Terragrunt](https://terragrunt.gruntwork.io/docs/getting-started/install/)
- AWS credentials with permissions for EC2, IAM, VPC, S3, and SSM
- An existing VPC and a **public** subnet (for the NAT gateway / bastion)
- A pre-created **S3 bucket** for Terraform state (set `state_bucket` in `common.hcl`)
- A local checkout of the **modules repo** for the bootstrap scripts and secrets:
  ```bash
  git clone https://github.com/migrx-io/terraform-aws-mgx.git
  cp terraform-aws-mgx/scripts/secrets.env.example terraform-aws-mgx/scripts/secrets.env  # then edit
  ```
  Set `scripts_path` / `secrets_file_path` in `common.hcl` to the **absolute**
  path of that checkout (Terragrunt runs Terraform from a cache dir, so relative
  paths won't resolve).

## Configure

Edit **`common.hcl`** — `vpc_id`, subnet CIDRs, `bastion.vpc_subnet`,
`state_bucket`, AMIs, and the absolute `scripts_path` / `secrets_file_path`.

> The pools use `raid_level = 0` (EBS cache), which requires a **single AZ** —
> keep `azs` to one entry. Per-pool sizing lives in `pools/_pool.hcl`; per-pool
> identity and buckets live in each `pools/<pool>/terragrunt.hcl`.

Pin `modules_ref` to a released tag when one is available (defaults to `main`).

## Deploy

Bring up everything in dependency order (network → pools → mgmt):

```bash
terragrunt run-all apply
```

Or one component at a time:

```bash
(cd network      && terragrunt apply)
(cd pools/pool1  && terragrunt apply)
(cd pools/pool2  && terragrunt apply)
(cd mgmt         && terragrunt apply)
```

Apply `mgmt` **after** the pools — it discovers them from SSM
(`/mgx/<cluster>/pools/`) on first provision. Re-running `mgmt` re-registers the
current set of pools.

## Day-2

| Task | Action |
|------|--------|
| Add a pool | Copy `pools/pool2` → `pools/pool3`, change `pool_name`/buckets, `(cd pools/pool3 && terragrunt apply)`, then re-apply `mgmt`. |
| Remove a pool | `(cd pools/pool3 && terragrunt destroy)`, then re-apply `mgmt`. |
| Scale a pool | Change `nodes_count`, `(cd pools/<pool> && terragrunt apply)`. |
| Tear down all | `terragrunt run-all destroy`. |

## Notes

- `backend.tf` and `provider.tf` are generated per unit by the root
  `terragrunt.hcl` and are git-ignored.
- `validate`/`plan` before the network exists work via the `network_mock`
  outputs in `common.hcl`.
