output "state_bucket" {
  description = "Name of the created state bucket — set this as state_bucket in ../common.hcl."
  value       = aws_s3_bucket.state.id
}

output "state_bucket_arn" {
  description = "ARN of the state bucket."
  value       = aws_s3_bucket.state.arn
}
