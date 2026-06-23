include "root" {
  path = find_in_parent_folders()
}

locals {
  common   = read_terragrunt_config(find_in_parent_folders("common.hcl")).locals
  defaults = read_terragrunt_config(find_in_parent_folders("_pool.hcl")).locals.pool_defaults
}

terraform {
  source = "${local.common.modules}/pool?ref=${local.common.modules_ref}"
}

dependency "network" {
  config_path                             = "../../network"
  mock_outputs                            = local.common.network_mock
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

inputs = merge(local.defaults, local.common.provision_inputs, {
  cluster   = local.common.cluster
  region    = local.common.region
  network   = dependency.network.outputs
  nodes_ami = local.common.nodes_ami

  pool_name   = "pool1"
  description = "Pool 1 (EBS RAID0 cache)"
  labels      = "name=pool-1,env=dev"

  s3_bucket_names        = ["mgxs3storage1"]
  s3_backup_bucket_names = ["mgxs3backup1"]
  s3_bucket_access_names = ["mgxs3storage2", "mgxs3backup2"] # access pool2's buckets
  s3_force_destroy       = true
})
