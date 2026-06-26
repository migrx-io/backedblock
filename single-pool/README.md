# single-pool starter (plain Terraform, SSH)

The **simplest** mgx-storage deployment: one storage pool, no management plane,
no SSM. Plain Terraform (no Terragrunt), and nodes are provisioned over SSH
through a bastion (`provision_mode = "ssh"`). Good for a single pool, a lab, or
a proof of concept.

For a multi-pool fleet with a management plane and SSM-based provisioning, use
[`../terragrunt-scale`](../terragrunt-scale) instead — see the
[top-level README](../README.md) for the comparison.

```
single-pool/
├── network/            # foundation: subnets, NAT, SG, key pair, bastion (apply first)
│   └── main.tf
├── pool/               # one storage pool, SSH-provisioned (apply second)
│   └── main.tf
└── secrets.env.example
```

Two stacks, each its own local state. `pool/` reads `network/`'s outputs from
`../network/terraform.tfstate`.

## Prerequisites

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.4
- AWS credentials with permissions for EC2, IAM, VPC, and S3 (no SSM needed)
- An existing VPC and a **public** subnet (for the bastion + NAT gateway)
- A prebaked **mgx AMI** (built by [mgx-packer](https://github.com/migrx-io)) for
  `nodes_ami`
- An SSH key pair on disk (`~/.ssh/id_rsa` / `~/.ssh/id_rsa.pub` by default)

## Step 1 — configure

Edit the values in `network/main.tf` (`vpc_id`, `azs`, subnet CIDRs,
`bastion.vpc_subnet`, `bastion.ami`, `key_name`, `ssh_public_key_path`) and
`pool/main.tf` (`pool_name`, `az`, `nodes_ami`, sizing, `s3_bucket_names`).
Pin the module `?ref=` to a released tag when one is available.

> The bastion is **required** here — SSH provisioning reaches the no-public-IP
> nodes through it. Lock `bastion.whitelist_ips` down to your own IP/CIDR.

## Step 2 — create the node secret

```bash
cd pool
cp ../secrets.env.example secrets.env   # then edit with real values (git-ignored)
cd ..
```

In SSH mode this file is uploaded to each node over the bastion at provision
time — it never leaves your machine for AWS storage.

## Step 3 — deploy

```bash
(cd network && terraform init && terraform apply)   # foundation + bastion
(cd pool    && terraform init && terraform apply)    # the pool (SSH-provisioned)
```

Tear down in reverse: `(cd pool && terraform destroy)` then
`(cd network && terraform destroy)`.

## Connecting to nodes

Nodes have no public IP; reach them through the bastion with SSH `-J`. Addresses
come straight from state:

```bash
BASTION=$(cd network && terraform output -raw bastion_public_ip)
(cd pool && terraform output node_mgmt_private_ips)   # node private IPs

ssh -J ubuntu@$BASTION ubuntu@<node-private-ip>
```

Default SSH user is `ubuntu` and the key is `ssh_private_key_path` in
`pool/main.tf`.
