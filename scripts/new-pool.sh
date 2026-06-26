#!/usr/bin/env bash
# Scaffold a new storage pool unit under pools/<name>/.
#
#   scripts/new-pool.sh <name> [az] [storage_bucket] [backup_bucket]
#
# Defaults: az = us-east-1a, storage = mgxs3storage-<name>, backup = mgxs3backup-<name>.
# The pool is pinned to `az` (must be one of azs in common.hcl). Sizing/cache
# come from pools/_pool.hcl. Cross-grant starts empty — edit s3_bucket_access_names
# in the generated file to allow access to other pools' buckets. mgmt
# auto-discovers every pool, so no mgmt edit is needed.
set -euo pipefail

# Repo root is the parent of this scripts/ directory.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAME="${1:-}"
if [ -z "$NAME" ]; then
  echo "usage: scripts/new-pool.sh <name> [az] [storage_bucket] [backup_bucket]" >&2
  exit 1
fi
if ! [[ "$NAME" =~ ^[a-zA-Z0-9-]+$ ]]; then
  echo "ERROR: pool name must be alphanumeric/dashes only (got '$NAME')." >&2
  exit 1
fi

AZ="${2:-us-east-1a}"
STORAGE="${3:-mgxs3storage-$NAME}"
BACKUP="${4:-mgxs3backup-$NAME}"
DESC="Pool $NAME (EBS RAID0 cache)"
LABELS="name=$NAME"

DIR="$ROOT/pools/$NAME"
if [ -e "$DIR" ]; then
  echo "ERROR: $DIR already exists." >&2
  exit 1
fi
mkdir -p "$DIR"

sed -e "s/__POOL_NAME__/$NAME/g" \
  -e "s/__AZ__/$AZ/g" \
  -e "s/__STORAGE__/$STORAGE/g" \
  -e "s/__BACKUP__/$BACKUP/g" \
  -e "s/__DESC__/$DESC/g" \
  -e "s/__LABELS__/$LABELS/g" \
  >"$DIR/terragrunt.hcl" <<'EOF'
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
  az        = "__AZ__" # pin this pool to a single AZ (EBS RAID0 cache)

  pool_name   = "__POOL_NAME__"
  description = "__DESC__"
  labels      = "__LABELS__"

  s3_bucket_names        = ["__STORAGE__"]
  s3_backup_bucket_names = ["__BACKUP__"]
  s3_bucket_access_names = [] # cross-grant: add other pools' bucket names here
  s3_force_destroy       = true
})
EOF

echo "Created pools/$NAME/terragrunt.hcl"
echo "  az             : $AZ"
echo "  storage bucket : $STORAGE"
echo "  backup bucket  : $BACKUP"
echo
echo "Next:"
echo "  1. (optional) edit s3_bucket_access_names in pools/$NAME/terragrunt.hcl to cross-grant."
echo "  2. (cd pools/$NAME && terragrunt apply)"
echo "  3. (cd mgmt && terragrunt apply)   # re-register pools (mgmt auto-discovers them)"
