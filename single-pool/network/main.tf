# Foundation / networking for the single-pool starter. Apply this FIRST.
# Plain Terraform, local state. The pool stack reads these outputs from this
# stack's state file (../network/terraform.tfstate).
#
# Edit the values below for your AWS account.

terraform {
  required_version = ">= 1.4"

  # Optional: use a remote backend instead of local state.
  # backend "s3" {
  #   bucket = "acme-tf-state"
  #   key    = "mgx/network/terraform.tfstate"
  #   region = "us-east-1"
  # }
}

provider "aws" {
  region = "us-east-1"
}

module "network" {
  # Pin ?ref= to a released tag once one is published.
  source = "git::https://github.com/migrx-io/terraform-aws-mgx.git//modules/network?ref=main"

  name_prefix = "backedblock"

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

  # SSM is the default for provisioning and operator access, so no bastion is
  # needed. Enable it only if you explicitly want SSH access to private nodes.
  bastion = {
    enable        = false
    vpc_subnet    = "subnet-06b5191fc3bf0caff" # public subnet (also hosts the NAT gateway)
    ami           = "ami-029f1e8b2d0665554"
    instance_type = "t4g.micro"
    whitelist_ips = []
  }

  ssh_public_key_path = "~/.ssh/id_rsa.pub"
  key_name            = "mgx-deployer-key"
}

# Re-export the module outputs so the pool stack can read them from this
# stack's state via terraform_remote_state.
output "azs" { value = module.network.azs }
output "mgmt_subnet_ids" { value = module.network.mgmt_subnet_ids }
output "storage_subnet_ids" { value = module.network.storage_subnet_ids }
output "internal_sg_id" { value = module.network.internal_sg_id }
output "key_name" { value = module.network.key_name }
output "bastion_enabled" { value = module.network.bastion_enabled }
output "bastion_public_ip" { value = module.network.bastion_public_ip }
output "bastion_private_ip" { value = module.network.bastion_private_ip }
