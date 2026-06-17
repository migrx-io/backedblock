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
    enable_metrics = true
    enable_grafana = false
  }
}
