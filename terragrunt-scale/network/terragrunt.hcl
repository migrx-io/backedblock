include "root" {
  path = find_in_parent_folders("root.hcl")
}

locals {
  common = read_terragrunt_config(find_in_parent_folders("common.hcl")).locals
}

terraform {
  source = "${local.common.modules}/network?ref=${local.common.modules_ref}"
}

inputs = {
  name_prefix          = "backedblock"
  vpc_id               = local.common.vpc_id
  azs                  = local.common.azs
  mgmt_subnet_cidrs    = local.common.mgmt_subnet_cidrs
  storage_subnet_cidrs = local.common.storage_subnet_cidrs
  bastion              = local.common.bastion
  key_name             = local.common.key_name
  ssh_public_key_path  = local.common.ssh_public_key_path
}
