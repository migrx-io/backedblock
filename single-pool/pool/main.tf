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
  nodes_ami           = "ami-0b14ba08cea3367cc" # prebaked node AMI (see docs/node-amis)
  nodes_instance_type = "c6gn.2xlarge"
  nodes_count         = 3

  # EBS RAID0 cache: pin the pool to a single AZ (EBS volumes are AZ-bound).
  az                    = "us-east-1a"
  raid_level            = 0
  nvme_node_disks_count = 10 # = total ebs_volumes count when raid_level = 0
  max_volumes_count     = 3
  r_cache_size_in_mib   = 97275
  rw_cache_size_in_mib  = 29183
  ebs_volumes = [{
    size       = 103
    type       = "gp3"
    iops       = 3000
    throughput = 125
    count      = 10
  }]

  # Write-back cache tuning. Pool-wide (every volume the pool serves); defaults
  # match the node image, so omit them unless you are tuning.
  # cache_flush_threads        = 10
  # cache_flush_interval       = 300
  # cache_flush_blocks         = 5
  # cache_flush_max_age        = 3000
  # cache_fill_threshold       = 100
  # cache_write_throttle_ms    = 50
  # cache_high_threshold       = 95
  # cache_low_threshold        = 85
  # cache_reclaim_scan_blocks  = 12800
  # cache_reclaim_scan_tries   = 20
  # cache_lru_percent          = 50
  # cache_reclaim_high_count   = 2
  # cache_reclaim_max_count    = 64
  # cache_max_overflow_percent = 5
  # cache_readahead_trigger    = 3
  # cache_readahead_blocks     = 32
  # cache_readahead_batch      = 4
  # cache_readahead_threads    = 8
  # cache_sync_interval        = 300
  # cache_persist_interval     = 1000
  # block_cache_flush_threads  = 30
  # block_cache_size           = 300
  # block_cache_threads        = 30

  # Storage / snapshot plugin config (storage.yaml); defaults match the node image.
  # storage_s3purge            = "yes"
  # cache_r_cache_size         = 4096
  # cache_rw_cache_size        = 1024
  # qos_rw_ios_per_sec         = 16000
  # qos_rw_mbytes_per_sec      = 250
  # qos_r_mbytes_per_sec       = 250
  # qos_w_mbytes_per_sec       = 250
  # snapshot_storage_class     = "GLACIER_IR"
  # snapshot_transfers         = 100
  # snapshot_checkers          = 64
  # snapshot_max_running       = 5
  # snapshot_max_increments    = 10

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
