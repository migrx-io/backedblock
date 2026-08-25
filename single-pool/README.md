# single-pool layout

The **simplest** backedblock.io deployment: one storage pool, no management plane,
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
- A prebaked **node AMI** for `nodes_ami` — the published ID for your region is
  in [Node AMIs](https://backedblock.io/docs/node-amis)
- An SSH key pair on disk (`~/.ssh/id_rsa` / `~/.ssh/id_rsa.pub` by default)

## Step 1 — configure

Edit the values in `network/main.tf` (`vpc_id`, `azs`, subnet CIDRs,
`bastion.vpc_subnet`, `bastion.ami`, `key_name`, `ssh_public_key_path`) and
`pool/main.tf` (`pool_name`, `az`, `nodes_ami`, sizing, `s3_bucket_names`).

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

## Managing the cluster with the CLI

Every node ships `mgx-cli` and the nodes are equivalent members of one cluster,
so whichever one you land on drives the whole thing. Two steps.

### Connect to any node

Nodes have no public IP; reach them through the bastion with SSH `-J`. Both
addresses come straight from state — the bastion's public IP, and the first node
out of the pool's list (needs `jq`):

```bash
BASTION=$(cd network && terraform output -raw bastion_public_ip)
NODE=$(cd pool && terraform output -json node_mgmt_private_ips | jq -r '.[0]')

ssh -J ubuntu@$BASTION ubuntu@$NODE
```

### Run `mgx-cli` and log in

```
$ mgx-cli
mgx-core:127.0.0.1:nologin> login admin --cluster main --ns main
Password:
 logged..
mgx-core:127.0.0.1:main:main:admin> cluster nodes list
```

It talks to the API on `127.0.0.1:8082`, the node you are on, and the password is
`MGX_GW_ADMIN_PASSWD` from `secrets.env`. Every command set — nodes, pools,
volumes, snapshots — is documented in the
[CLI reference](https://backedblock.io/docs/cli).

### Optional: port-forwarding Grafana (port 3000) through the bastion

Only when the pool was applied with `enable_metrics` and `enable_grafana`.
Grafana runs on a node with no public IP, so tunnel its port `3000` to your
machine through the bastion. Use SSH `-J` to hop the bastion and `-L` to forward
the local port.

**First identify the VIP node.** Grafana always runs on the cluster's VIP node,
and the VIP can move between nodes, so don't assume a fixed IP — look it up.
SSH into any node (see above), open the CLI,
and query the cluster:

```
mgx-core:127.0.0.1:nologin> login admin --cluster main --ns main
Password:
 logged..
mgx-core:127.0.0.1:main:main:admin> cluster list
 [
    {
        "cluster": "main",
        "vip_host": {
            "ip": "172.31.102.214",          # <- node currently holding the VIP / Grafana
            "uuid": "4a11e6fb-d615-53ae-ad1e-fc294f6b7048",
            "vip": "127.0.0.1"
        }
    }
]
```

Use the `vip_host.ip` value as `VIP` below. (`cluster nodes list` shows all
nodes and their IPs if you need to cross-reference.) Because the VIP can fail
over to another node, re-check `cluster list` if the tunnel stops working.

Then forward the port through the bastion to that node:

```bash
BASTION=$(cd network && terraform output -raw bastion_public_ip)
VIP=<vip-node-ip>

ssh -N -J ubuntu@$BASTION -L 3000:localhost:3000 ubuntu@$VIP
```

Then open <http://localhost:3000> in your browser. `-N` keeps the tunnel open
without running a remote command; leave the terminal running and Ctrl-C to close
it. If local port `3000` is already taken, map a different one, e.g.
`-L 3001:localhost:3000`.
