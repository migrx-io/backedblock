# A single storage pool — plain Terraform, SSH provisioning, no mgmt, no SSM.
# Apply this AFTER ../network. To run more than one pool, copy this directory
# (each pool is an independent state) or just bump nodes_count.

terraform {
  required_version = ">= 1.4"

  # backend "s3" {
  #   bucket = "acme-tf-state"
  #   key    = "mgx/pool/terraform.tfstate"
  #   region = "us-east-1"
  # }
}

provider "aws" {
  region = "us-east-1"
}

# Read the foundation from the network stack's state.
data "terraform_remote_state" "network" {
  backend = "local" # match the backend the network stack uses
  config = {
    path = "../network/terraform.tfstate"
  }
}

module "pool" {
  # Pin ?ref= to a released tag once one is published.
  source = "git::https://github.com/migrx-io/terraform-aws-mgx.git//modules/pool?ref=main"

  cluster   = "main"
  pool_name = "pool1"
  region    = "us-east-1"
  network   = data.terraform_remote_state.network.outputs

  description         = "Single pool"
  labels              = "name=pool-1,env=dev"
  nodes_ami           = "ami-057115ae19198f552" # prebaked node AMI (see docs/node-amis)
  nodes_instance_type = "c6gn.2xlarge"
  nodes_count         = 3

  # EBS RAID0 cache: pin the pool to a single AZ (EBS volumes are AZ-bound).
  az                    = "us-east-1a"
  raid_level            = 0
  nvme_node_disks_count = 10 # = total ebs_volumes count when raid_level = 0
  max_volumes_count     = 3
  r_cache_size_in_mib   = 97275
  rw_cache_size_in_mib  = 48638
  ebs_volumes = [{
    size       = 103
    type       = "gp3"
    iops       = 3000
    throughput = 125
    count      = 10
  }]

  # Write-back cache tuning. Pool-wide (every volume the pool serves); defaults
  # match the node image, so omit them unless you are tuning.
  # cache_flush_threads       = 20
  # cache_flush_interval      = 1000
  # cache_flush_blocks        = 5
  # cache_flush_max_age       = 3000
  # cache_fill_threshold      = 60
  # block_cache_flush_threads = 30
  # block_cache_size          = 300 # MiB
  # block_cache_threads       = 30

  s3_bucket_names        = ["mgxs3storage1"]
  s3_backup_bucket_names = ["mgxs3backup1"]
  s3_force_destroy       = true

  enable_metrics = true
  enable_grafana = true
  # Standalone pool (no mgmt): each node scrapes every peer for a full per-pool
  # metrics replica. (Set false only when attaching the pool to a mgmt plane.)
  cross_peer_scrape = true

  # --- provisioning: SSH over the bastion (no SSM) ---------------------------
  # provision_mode defaults to "ssh": Terraform connects through the bastion,
  # uploads secrets.env from this dir, writes pool_info.json + ip lists, and runs
  # the baked setup-node.sh. Create the secret first:
  #   cp ../secrets.env.example secrets.env   # then fill in real values
  provision_mode       = "ssh"
  ssh_user             = "ubuntu"
  ssh_private_key_path = "~/.ssh/id_rsa"
  secrets_file_path    = "secrets.env"
}

output "node_mgmt_private_ips" { value = module.pool.node_mgmt_private_ips }
output "node_data_private_ips" { value = module.pool.node_data_private_ips }
