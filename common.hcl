# Shared values for the whole deployment — the single source of truth. Every
# unit reads them via read_terragrunt_config(find_in_parent_folders("common.hcl")).
locals {
  # --- module source ---------------------------------------------------------
  # Pin modules_ref to a release tag once one is published.
  modules     = "git::https://github.com/migrx-io/terraform-aws-mgx.git//modules"
  modules_ref = "main"

  # --- global ----------------------------------------------------------------
  region       = "us-east-1"
  cluster      = "main"
  state_bucket = "mgx-storage-tf-state" # created by the bootstrap/ stack

  # --- network ---------------------------------------------------------------
  # azs is ONLY for the network: it builds one mgmt + one storage subnet per AZ.
  # Each pool then pins itself to a single AZ (its `az`, set in the pool's
  # terragrunt.hcl) — EBS RAID0 cache volumes are AZ-bound. One cidr per AZ, in
  # the same order as azs.
  vpc_id = "vpc-095dc0635c6244fe3"
  azs    = ["us-east-1a", "us-east-1b", "us-east-1c"]
  mgmt_subnet_cidrs = [
    "172.31.96.0/20",  # us-east-1a
    "172.31.112.0/20", # us-east-1b
    "172.31.128.0/20", # us-east-1c
  ]
  storage_subnet_cidrs = [
    "172.31.144.0/20", # us-east-1a
    "172.31.160.0/20", # us-east-1b
    "172.31.176.0/20", # us-east-1c
  ]

  # This example provisions agentlessly via SSM (see below), so no bastion is
  # needed. vpc_subnet is kept because the NAT gateway is placed in it (and it
  # lets you flip enable = true if you ever want SSH access).
  bastion = {
    enable        = false
    vpc_subnet    = "subnet-06b5191fc3bf0caff" # public subnet (used for the NAT gateway)
    ami           = "ami-029f1e8b2d0665554"
    instance_type = "t4g.micro"
    whitelist_ips = ["0.0.0.0/0"]
  }

  key_name            = "mgx-deployer-key"
  ssh_public_key_path = "~/.ssh/id_rsa.pub"

  # --- node image ------------------------------------------------------------
  # Prebaked mgx AMI (built by mgx-packer); provisioning runs the baked
  # setup-node.sh in place.
  nodes_ami = "ami-029f1e8b2d0665554"

  # --- provisioning ----------------------------------------------------------
  # This Terragrunt example always provisions via SSM (agentless, no bastion).
  # setup-node.sh is driven by SSM Run Command; nodes fetch secrets.env content
  # from the SSM SecureString at secrets_ssm_path (the NAT gateway gives them the
  # egress they need to the SSM endpoints).
  #
  # (The terraform-aws-mgx modules also support provision_mode = "ssh"; this
  # starter just standardizes on ssm.)
  #
  # Store secrets.env content in this SSM SecureString before applying:
  #   aws ssm put-parameter --type SecureString \
  #     --name /mgx/${local.cluster}/secrets --value file://secrets.env
  secrets_ssm_path = "/mgx/${local.cluster}/secrets"

  provision_inputs = {
    provision_mode   = "ssm"
    secrets_ssm_path = local.secrets_ssm_path
  }
}
