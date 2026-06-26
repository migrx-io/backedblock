include "root" {
  path = find_in_parent_folders("root.hcl")
}

locals {
  common   = read_terragrunt_config(find_in_parent_folders("common.hcl")).locals
  defaults = read_terragrunt_config(find_in_parent_folders("_pool.hcl")).locals.pool_defaults
}

terraform {
  source = "${local.common.modules}/pool?ref=${local.common.modules_ref}"
}

dependency "network" {
  config_path = "../../network"
}

inputs = merge(local.defaults, local.common.provision_inputs, {
  cluster   = local.common.cluster
  region    = local.common.region
  network   = dependency.network.outputs
  nodes_ami = local.common.nodes_ami
  az        = "us-east-1b" # pin this pool to a single AZ (EBS RAID0 cache)

  pool_name   = "pool2"
  description = "Pool 2 (EBS RAID0 cache)"
  labels      = "name=pool-2,env=dev"

  s3_bucket_names        = ["mgxs3storage2"]
  s3_backup_bucket_names = ["mgxs3backup2"]
  s3_bucket_access_names = ["mgxs3storage1", "mgxs3backup1"] # cross-grant: access pool1's buckets
  s3_force_destroy       = true
})
