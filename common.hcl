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

  # --- provisioning (ssh / bastion mode) -------------------------------------
  # scripts/ and secrets.env come from the modules repo (terraform-aws-mgx).
  # Terragrunt runs Terraform from a cache directory, so these MUST be ABSOLUTE
  # paths to a local checkout.
  scripts_path         = "/ABSOLUTE/PATH/TO/terraform-aws-mgx/scripts"
  secrets_file_path    = "/ABSOLUTE/PATH/TO/terraform-aws-mgx/scripts/secrets.env"
  ssh_user             = "ubuntu"
  ssh_private_key_path = "~/.ssh/id_rsa"

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
