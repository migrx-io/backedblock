include "root" {
  path = find_in_parent_folders("root.hcl")
}

locals {
  common = read_terragrunt_config(find_in_parent_folders("common.hcl")).locals

  # Auto-discover every pool unit so adding a pool needs no edit here.
  pool_dirs = [
    for f in fileset("${get_terragrunt_dir()}/../pools", "*/terragrunt.hcl") :
    "../pools/${dirname(f)}"
  ]
}

terraform {
  source = "${local.common.modules}/mgmt?ref=${local.common.modules_ref}"
}

dependency "network" {
  config_path = "../network"
}

# mgmt discovers the pools from SSM at apply time, so it must apply AFTER them.
# Ordering only (no outputs consumed) — the list is built from the pools/ dir.
dependencies {
  paths = local.pool_dirs
}

inputs = merge(local.common.provision_inputs, {
  cluster             = local.common.cluster
  region              = local.common.region
  network             = dependency.network.outputs
  nodes_ami           = local.common.nodes_ami
  nodes_instance_type = "t4g.xlarge"
  nodes_count         = 3
  enable_metrics      = true
  enable_grafana      = false
})
