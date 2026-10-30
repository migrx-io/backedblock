package config

import (
	"fmt"
	"strings"
)

const (
	DefaultModulesRef   = "main"
	DefaultInstanceType = "c6gn.2xlarge"
	DefaultMgmtType     = "t4g.xlarge"
	DefaultNodesCount   = 3
)

// DefaultPool is a 3-node pool with a 10 x 103 GiB gp3 RAID0 cache, the same
// sizing as the published blueprints. Bucket names include the deployment name
// and account ID because S3 names are global.
func DefaultPool(deployment, account, name, az string) Pool {
	slug := strings.ToLower(name)
	return Pool{
		Name:             name,
		AZ:               az,
		Description:      fmt.Sprintf("Pool %s (EBS RAID0 cache)", name),
		Labels:           "name=" + name,
		NodesCount:       DefaultNodesCount,
		InstanceType:     DefaultInstanceType,
		RaidLevel:        0,
		MaxVolumesCount:  3,
		RCacheSizeInMiB:  97275,
		RWCacheSizeInMiB: 29183,
		EBSVolumes: []EBSVolume{{
			Size: 103, Type: "gp3", IOPS: 3000, Throughput: 125, Count: 10,
		}},
		S3BucketNames:   []string{fmt.Sprintf("%s-%s-%s-data", deployment, account, slug)},
		S3BackupBuckets: []string{fmt.Sprintf("%s-%s-%s-backup", deployment, account, slug)},
		S3ForceDestroy:  true,
		EnableMetrics:   true,
	}
}

// DefaultMgmt is a 3-node management plane.
func DefaultMgmt() *Mgmt {
	return &Mgmt{NodesCount: DefaultNodesCount, InstanceType: DefaultMgmtType, EnableMetrics: true, EnableGrafana: true}
}

// StateBucketName is the default Terraform state bucket for a deployment.
func StateBucketName(deployment, account, region string) string {
	return fmt.Sprintf("%s-tfstate-%s-%s", deployment, account, region)
}
