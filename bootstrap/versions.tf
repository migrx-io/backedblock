terraform {
  required_version = ">= 1.4"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0, < 6.0"
    }
  }

  # Bootstrap uses LOCAL state (it creates the bucket the other stacks store
  # their state in — chicken/egg). Keep bootstrap/terraform.tfstate. Optionally,
  # after the first apply, migrate it into the bucket with a backend "s3" block
  # + `terraform init -migrate-state`.
}
