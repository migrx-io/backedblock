# single-pool layout

The **simplest** backedblock.io deployment: one storage pool, no management plane,
and plain Terraform (no Terragrunt). Provisioning and operator access use AWS
Systems Manager (SSM) by default, so the bastion is disabled unless you
explicitly enable SSH. Good for a single pool, a lab, or a proof of concept.

For a multi-pool fleet with a management plane, use
[`../terragrunt-scale`](../terragrunt-scale) instead — see the
[top-level README](../README.md) for the comparison.

```
single-pool/
├── network/            # foundation: subnets, NAT, SG, optional bastion (apply first)
│   └── main.tf
├── pool/               # one storage pool, SSM-provisioned (apply second)
│   └── main.tf
└── secrets.env.example
```

Two stacks, each its own local state. `pool/` reads `network/`'s outputs from
`../network/terraform.tfstate`.

## Prerequisites

- [Terraform](https://developer.hashicorp.com/terraform/downloads) >= 1.4
- AWS credentials with permissions for EC2, IAM, VPC, S3, and SSM
- An existing VPC and a **public** subnet (for the NAT gateway)
- A prebaked **node AMI** for `nodes_ami` — the published ID for your region is
  in [Node AMIs](https://backedblock.io/docs/node-amis)
- AWS CLI and the [Session Manager plugin](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager-working-with-install-plugin.html)
  configured for the deployment region

## Step 1 — configure

Edit the values in `network/main.tf` (`vpc_id`, `azs`, subnet CIDRs,
`bastion.vpc_subnet`) and `pool/main.tf` (`pool_name`, `az`, `nodes_ami`, sizing,
`s3_bucket_names`). The bastion is disabled by default. To enable SSH access,
set `bastion.enable = true` and restrict `bastion.whitelist_ips` to trusted
addresses. For SSH, also provide the key pair configured in `network/main.tf`.

Optional pool settings ship commented out in `pool/main.tf` — uncomment to override
the module defaults:

| Option | Default | Values |
|---|---|---|
| `cache_filesystem` | `"ext4"` | cache disk filesystem: `"xfs"` or `"ext4"` |
| `scheduler_node_down_wait` | `300` | seconds a node must stay DOWN before it is auto-drained and its volumes fail over |

> SSM is used for provisioning and interactive access. The nodes need outbound
> connectivity to the SSM endpoints; the NAT gateway provides it. A bastion is
> optional and only needed if you choose SSH access.

## Step 2 — create the node secret

```bash
cd pool
cp ../secrets.env.example secrets.env   # then edit with real values (git-ignored)
cd ..
```

Store this file in Parameter Store as a SecureString. It is fetched by the
instances during provisioning and does not enter Terraform state:

```bash
aws ssm put-parameter --region us-east-1 --type SecureString \
  --name /mgx/main/secrets --value file://secrets.env --overwrite
```

## Step 3 — deploy

```bash
(cd network && terraform init && terraform apply)   # foundation + NAT
(cd pool    && terraform init && terraform apply)    # the pool (SSM-provisioned)
```

Tear down in reverse: `(cd pool && terraform destroy)` then
`(cd network && terraform destroy)`.

## Managing the cluster with the CLI

Every node ships `mgx-cli` and the nodes are equivalent members of one cluster,
so whichever one you land on drives the whole thing. Two steps.

### Connect to any node with Session Manager

Nodes have no public IP. Find a node's instance ID in the EC2 console or
Systems Manager managed-nodes list, then start a shell session:

```bash
aws ssm start-session --region us-east-1 --target i-INSTANCE_ID
```

Run `mgx-cli` in the session and log in:

```
$ mgx-cli
mgx-core:127.0.0.1:nologin> login admin --cluster main --ns main
Password:
 logged..
mgx-core:127.0.0.1:main:main:admin> cluster nodes list
```

It talks to the API on `127.0.0.1:8082` on that node, and the password is
`MGX_GW_ADMIN_PASSWD` from `secrets.env`. Every command set — nodes, pools,
volumes, snapshots — is documented in the
[CLI reference](https://backedblock.io/docs/cli).

### Optional: port-forward Grafana (port 3000) with Session Manager

Only when the pool was applied with `enable_metrics` and `enable_grafana`.
Grafana runs on the cluster's VIP node, which has no public IP. Session Manager
forwards its port `3000` to your machine without opening inbound access.

**First identify the VIP node.** Grafana always runs on the cluster's VIP node,
and the VIP can move between nodes, so don't assume a fixed IP — look it up.
Open a Session Manager shell to any node (see above), run `mgx-cli`, and query
the cluster:

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

Use the `vip_host.ip` value to find the EC2 instance ID of the current VIP node:

```bash
VIP=172.31.102.214
INSTANCE_ID=$(aws ec2 describe-instances --region us-east-1 \
  --filters "Name=private-ip-address,Values=$VIP" \
  --query 'Reservations[].Instances[].InstanceId' --output text)
```

Forward the port:

```bash
aws ssm start-session --region us-east-1 --target "$INSTANCE_ID" \
  --document-name AWS-StartPortForwardingSession \
  --parameters '{"portNumber":["3000"],"localPortNumber":["3000"]}'
```

Then open <http://localhost:3000> in your browser. Keep the command running
while using Grafana and press Ctrl-C to close the tunnel. If the VIP moves, look
up its new IP and start a new session. If local port `3000` is already taken,
set `localPortNumber` to another port such as `3001`.
