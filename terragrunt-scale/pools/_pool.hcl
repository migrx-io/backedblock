# Shared defaults for EBS-cache storage pools (raid_level = 0). Each pool unit
# merges these with its own pool_name / buckets / description.
locals {
  pool_defaults = {
    nodes_instance_type   = "m8gb.xlarge"
    nodes_count           = 3
    raid_level            = 0
    nvme_node_disks_count = 10 # must equal the total ebs_volumes count
    max_volumes_count     = 10
    r_cache_size_in_mib   = 90000
    rw_cache_size_in_mib  = 10000
    ebs_volumes = [{
      size       = 100
      type       = "gp3"
      iops       = 3000
      throughput = 125
      count      = 10
    }]
    # Write-back cache tuning, pool-wide. Defaults match the node image;
    # uncomment to tune every pool at once.
    # cache_flush_threads       = 20
    # cache_flush_interval      = 1000
    # cache_flush_blocks        = 5
    # cache_flush_max_age       = 3000
    # cache_fill_threshold      = 60
    # block_cache_flush_threads = 30
    # block_cache_size          = 300 # MiB
    # block_cache_threads       = 30

    enable_metrics = true
    enable_grafana = false
    # false = each node scrapes only itself and mgmt federates every node (no
    # node-selection SPOF). Set true only for a standalone pool not on mgmt.
    cross_peer_scrape = false
  }
}
