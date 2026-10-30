# Code review guide

Use this guide when reviewing changes in this repository. Review the current
working tree, including staged, unstaged, and untracked files. Compare the
implementation with `main` when the request is to review a branch or rewrite.
Do not modify code while performing a review.

## Review priorities

Look for concrete defects that could cause incorrect infrastructure changes,
data loss, security exposure, broken installation, or an unusable agent/API
contract. Follow behavior across the CLI, generated Terraform, AWS SDK calls,
and the Terraform modules/examples; do not rely on comments or README claims
without checking the implementation.

For `bbctl`, pay particular attention to:

- **Approval and side effects:** `plan` must not mutate AWS. `apply` must get
  approval before creating billable infrastructure, including the state
  bucket. `destroy` and pool removal can delete buckets and volume data. Check
  behavior with and without a TTY, `--json`, `--auto-approve`, and `--stack`.
- **State and retries:** state bucket/object checks must distinguish missing
  resources from permission/network errors. Partial apply, destroy, and pool
  removal should leave clear, recoverable state and safe retry instructions.
- **Generated Terraform:** verify module inputs/outputs against the original
  examples and module contract; check dependency/order assumptions, backend
  initialization, state keys, secrets handling, and changes to pool/mgmt
  relationships.
- **AWS discovery and access:** validate VPC, route table, subnet, AZ, AMI,
  instance type, SSM, and S3 behavior, including API failures and pagination.
- **Agent contract:** with `--json`, stdout must contain one JSON document;
  progress belongs on stderr. Errors need the documented JSON shape and stable
  exit code. Confirm that commands described as safe to retry really are.
- **Install and release:** compare GoReleaser artifact names/platforms with
  installer URLs, archive formats, checksums, and README instructions.
- **Config and file operations:** validate malformed and boundary configs,
  path handling, atomic writes, permissions, and accidental overwrites.

## Findings format

Report only actionable findings supported by the current code. Order them by
severity and include:

1. Severity (`P1`, `P2`, or `P3`) and the defect in one sentence.
2. A short explanation of the triggering scenario and practical impact.
3. A clickable local file link with the narrowest relevant line reference.

Do not report style preferences, speculative risks, or issues already fixed in
the current tree. If a previous finding was fixed, say so briefly. If there
are no confirmed findings, state that clearly and mention whether tests were
run. Do not run tests unless the user asks for verification.

## Suggested invocation

“Review the current changes using [REVIEWING.md](REVIEWING.md). Compare with
`main`, inspect staged/unstaged/untracked files, and report findings only.”
