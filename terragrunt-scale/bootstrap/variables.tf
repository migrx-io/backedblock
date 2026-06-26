variable "region" {
  description = "AWS region for the state bucket. Match region in ../common.hcl."
  type        = string
  default     = "us-east-1"
}

variable "state_bucket" {
  description = "Name of the S3 bucket to create for Terraform/Terragrunt state. Must match state_bucket in ../common.hcl."
  type        = string
}
