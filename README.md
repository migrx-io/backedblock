# mgx-storage starters

Two ready-to-edit blueprints for deploying [mgx-storage](https://migrx.io) on
AWS over the [`migrx-io/terraform-aws-mgx`](https://github.com/migrx-io/terraform-aws-mgx)
modules. Pick the one that matches your scale — they're independent, each in its
own directory.

| | [`single-pool/`](single-pool) | [`terragrunt-scale/`](terragrunt-scale) |
|---|---|---|
| **Use when** | one pool, a lab, a PoC | many pools, a real fleet |
| **Tooling** | plain Terraform | [Terragrunt](https://terragrunt.gruntwork.io/) (DRY, `run --all`) |
| **Topology** | 1 storage pool | management plane + N storage pools |
| **Provisioning** | SSH via bastion | SSM Run Command (agentless) |
| **Secrets** | local `secrets.env`, uploaded over SSH | SSM SecureString |
| **Pool discovery** | n/a (single pool) | pools auto-discovered via SSM Parameter Store |
| **State** | local (per stack) | S3 backend (per unit) |
| **Bastion** | required (it's the provisioning path) | optional — only for SSH access to nodes |

Both pull the modules straight from GitHub and expect a prebaked **mgx AMI**
(built by [mgx-packer](https://github.com/migrx-io)) for `nodes_ami`.

## Which one?

- **Just want one pool running?** → [`single-pool/`](single-pool). Two `terraform
  apply`s (network, then pool), no Terragrunt, no SSM. Provisioning happens over
  SSH through a bastion.

- **Running a fleet?** → [`terragrunt-scale/`](terragrunt-scale). Each pool is a
  ~25-line `terragrunt.hcl`, `run --all` builds the dependency graph, a mgmt
  plane auto-discovers every pool, and provisioning is agentless over SSM (no
  bastion needed — though you can enable one purely for SSH access).

See each directory's `README.md` for full setup steps.
