# Creates and hardens the S3 bucket that holds Terraform/Terragrunt state.
# Run this FIRST: `terraform init && terraform apply`. Then set the same name as
# state_bucket in ../common.hcl and deploy the rest with Terragrunt.

resource "aws_s3_bucket" "state" {
  bucket = var.state_bucket

  # Guard against accidental deletion of the bucket holding all state.
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket_versioning" "state" {
  bucket = aws_s3_bucket.state.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "state" {
  bucket = aws_s3_bucket.state.id
  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
    bucket_key_enabled = true
  }
}

resource "aws_s3_bucket_public_access_block" "state" {
  bucket                  = aws_s3_bucket.state.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}
