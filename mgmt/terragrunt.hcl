include "root" {
  path = find_in_parent_folders()
}

locals {
  common = read_terragrunt_config(find_in_parent_folders("common.hcl")).locals
}

terraform {
  source = "${local.common.modules}/mgmt?ref=${local.common.modules_ref}"
}

dependency "network" {
  config_path                             = "../network"
  mock_outputs                            = local.common.network_mock
  mock_outputs_allowed_terraform_commands = ["validate", "plan", "init"]
}

# mgmt discovers the pools from SSM at apply time, so it must apply AFTER them.
dependencies {
  paths = ["../pools/pool1", "../pools/pool2"]
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
