# Shared defaults for EBS-cache storage pools (raid_level = 0). Each pool unit
# merges these with its own pool_name / buckets / description.
locals {
  pool_defaults = {
    nodes_instance_type   = "c6gn.2xlarge"
    nodes_count           = 3
    raid_level            = 0
    nvme_node_disks_count = 10 # must equal the total ebs_volumes count
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
    # Write-back cache tuning, pool-wide. Defaults match the node image;
    # uncomment to tune every pool at once.
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

    enable_metrics = true
    enable_grafana = false
    # false = each node scrapes only itself and mgmt federates every node (no
    # node-selection SPOF). Set true only for a standalone pool not on mgmt.
    cross_peer_scrape = false
  }
}
