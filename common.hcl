# Shared values for the whole deployment. Edit these for your environment;
# every unit reads them via read_terragrunt_config(find_in_parent_folders("common.hcl")).
locals {
  # --- module source ---------------------------------------------------------
  # Pin modules_ref to a release tag once one is published.
  modules     = "git::https://github.com/migrx-io/terraform-aws-mgx.git//modules"
  modules_ref = "main"

  # --- global ----------------------------------------------------------------
  region       = "us-east-1"
  cluster      = "main"
  state_bucket = "CHANGE-ME-mgx-tf-state" # pre-create this S3 bucket

  # --- network ---------------------------------------------------------------
  # NOTE: a SINGLE AZ is required for EBS RAID0 cache pools (raid_level = 0),
  # because EBS volumes are AZ-bound.
  vpc_id               = "vpc-xxxxxxxxxxxxxxxxx"
  azs                  = ["us-east-1a"]
  mgmt_subnet_cidrs    = ["172.31.96.0/20"]
  storage_subnet_cidrs = ["172.31.144.0/20"]

  bastion = {
    enable        = true
    vpc_subnet    = "subnet-xxxxxxxxxxxxxxxxx" # public subnet
    ami           = "ami-029f1e8b2d0665554"
    instance_type = "t4g.micro"
    whitelist_ips = ["0.0.0.0/0"]
  }

  key_name            = "mgx-deployer-key"
  ssh_public_key_path = "~/.ssh/id_rsa.pub"

  # --- node image ------------------------------------------------------------
  nodes_ami = "ami-029f1e8b2d0665554"

  # --- provisioning ----------------------------------------------------------
  # Transport used to run the bootstrap scripts on each node:
  #   "ssm" - agentless. Nodes pull the scripts from scripts_url and read
  #           secrets from SSM themselves. No local checkout, no bastion.
  #   "ssh" - Terraform uploads a LOCAL scripts dir + secrets.env via the
  #           bastion. Requires absolute paths to a terraform-aws-mgx checkout.
  provision_mode = "ssm"

  # ssm-mode inputs (used when provision_mode = "ssm"). Host a gzipped tarball
  # of the modules repo's scripts/ dir and store secrets.env in an SSM
  # SecureString parameter; nodes fetch both on first provision.
  #   tar czf mgx-scripts.tgz -C terraform-aws-mgx scripts   # then upload it
  #   aws ssm put-parameter --type SecureString \
  #     --name /mgx/${local.cluster}/secrets --value file://scripts/secrets.env
  scripts_url      = "https://CHANGE-ME/mgx-scripts.tgz"
  secrets_ssm_path = "/mgx/${local.cluster}/secrets"

  # ssh-mode inputs (used when provision_mode = "ssh"). scripts/ and secrets.env
  # come from the modules repo (terraform-aws-mgx). Terragrunt runs Terraform
  # from a cache directory, so these MUST be ABSOLUTE paths to a local checkout.
  scripts_path         = "/ABSOLUTE/PATH/TO/terraform-aws-mgx/scripts"
  secrets_file_path    = "/ABSOLUTE/PATH/TO/terraform-aws-mgx/scripts/secrets.env"
  ssh_user             = "ubuntu"
  ssh_private_key_path = "~/.ssh/id_rsa"

  # Computed provisioning inputs for the mgmt/pool units — only the keys for the
  # selected mode are sent, so units never carry the other mode's placeholders.
  provision_inputs = merge(
    { provision_mode = local.provision_mode },
    local.provision_mode == "ssm" ? {
      scripts_url      = local.scripts_url
      secrets_ssm_path = local.secrets_ssm_path
      } : {
      scripts_path         = local.scripts_path
      secrets_file_path    = local.secrets_file_path
      ssh_user             = local.ssh_user
      ssh_private_key_path = local.ssh_private_key_path
    },
  )

  # --- mock outputs for `validate`/`plan` before the network is applied ------
  network_mock = {
    azs                  = ["us-east-1a"]
    mgmt_subnet_ids      = ["subnet-mock"]
    storage_subnet_ids   = ["subnet-mock"]
    mgmt_subnet_cidrs    = ["172.31.96.0/20"]
    storage_subnet_cidrs = ["172.31.144.0/20"]
    internal_sg_id       = "sg-mock"
    key_name             = "mock"
    bastion_enabled      = true
    bastion_public_ip    = "203.0.113.10"
    bastion_private_ip   = "10.0.0.10"
  }
}
