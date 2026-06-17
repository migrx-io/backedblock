# Root config: generates the S3 backend and AWS provider for every unit, and
# derives a unique state key per unit. Child units `include "root"` this file.

locals {
  common = read_terragrunt_config(find_in_parent_folders("common.hcl")).locals
}

remote_state {
  backend = "s3"

  generate = {
    path      = "backend.tf"
    if_exists = "overwrite"
  }

  config = {
    bucket  = local.common.state_bucket
    key     = "${path_relative_to_include()}/terraform.tfstate"
    region  = local.common.region
    encrypt = true
    # dynamodb_table = "mgx-tf-locks"   # enable for state locking
  }
}

generate "provider" {
  path      = "provider.tf"
  if_exists = "overwrite"
  contents  = <<-EOF
    provider "aws" {
      region = "${local.common.region}"
    }
  EOF
}
