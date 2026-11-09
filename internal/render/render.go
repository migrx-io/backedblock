// Package render turns a bbctl.yaml into Terraform root modules ("stacks"),
// one per state: network, each pool, and the optional mgmt plane. Stacks are
// written as Terraform JSON (main.tf.json) and call the terraform-aws-mgx
// modules from a local directory, so neither git nor a registry is needed.
package render

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/migrx-io/backedblock/internal/config"
)

// Kind of a stack, which also fixes the apply order.
type Kind string

const (
	KindNetwork Kind = "network"
	KindPool    Kind = "pool"
	KindMgmt    Kind = "mgmt"
)

type Stack struct {
	// ID is unique and stable: "network", "pool/<name>", "mgmt".
	ID   string `json:"id"`
	Kind Kind   `json:"kind"`
	// Pool is set for pool stacks.
	Pool string `json:"pool,omitempty"`
	Dir  string `json:"dir"`
	// StateKey is the object key of the stack's state in the state bucket.
	StateKey string `json:"state_key"`
}

// Stacks lists the stacks of a deployment in apply order: network, pools,
// then mgmt (mgmt discovers the pools from SSM, so it must come last).
func Stacks(c *config.Config, workDir string) []Stack {
	st := []Stack{{
		ID: "network", Kind: KindNetwork,
		Dir:      filepath.Join(workDir, "stacks", "network"),
		StateKey: c.Name + "/network.tfstate",
	}}
	for _, p := range c.Pools {
		st = append(st, Stack{
			ID: "pool/" + p.Name, Kind: KindPool, Pool: p.Name,
			Dir:      filepath.Join(workDir, "stacks", "pools", p.Name),
			StateKey: c.Name + "/pools/" + p.Name + ".tfstate",
		})
	}
	if c.Mgmt != nil {
		st = append(st, Stack{
			ID: "mgmt", Kind: KindMgmt,
			Dir:      filepath.Join(workDir, "stacks", "mgmt"),
			StateKey: c.Name + "/mgmt.tfstate",
		})
	}
	return st
}

// Inputs are values resolved at run time rather than read from the config.
type Inputs struct {
	// ModulesDir is the root of a terraform-aws-mgx checkout (contains modules/).
	ModulesDir string
	// SSHPublicKeyPath is the key registered as the EC2 key pair.
	SSHPublicKeyPath string
}

// Write renders every stack into its directory. Stacks of pools removed from
// the config are left alone: their state still exists and must be destroyed
// explicitly.
func Write(c *config.Config, stacks []Stack, in Inputs) error {
	for _, s := range stacks {
		doc, err := stackDoc(c, s, stacks, in)
		if err != nil {
			return err
		}
		if err := writeDoc(s.Dir, doc); err != nil {
			return err
		}
	}
	return nil
}

// WriteLocal renders stack s into dir with a local backend whose state lives
// in dir. Used to plan a deployment whose state bucket does not exist yet;
// the result must never be applied.
func WriteLocal(c *config.Config, s Stack, stacks []Stack, in Inputs, dir string) error {
	s.Dir = dir
	doc, err := stackDoc(c, s, stacks, in)
	if err != nil {
		return err
	}
	doc["terraform"].(obj)["backend"] = obj{"local": obj{"path": "terraform.tfstate"}}
	return writeDoc(dir, doc)
}

func writeDoc(dir string, doc obj) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "main.tf.json"), append(b, '\n'), 0o644)
}

type obj = map[string]any

func stackDoc(c *config.Config, s Stack, all []Stack, in Inputs) (obj, error) {
	src, err := moduleSource(s.Dir, in.ModulesDir, string(s.Kind))
	if err != nil {
		return nil, err
	}
	doc := obj{
		"terraform": obj{
			"required_version": ">= 1.10",
			"backend": obj{"s3": obj{
				"bucket":       c.State.Bucket,
				"key":          s.StateKey,
				"region":       c.Region,
				"encrypt":      true,
				"use_lockfile": true,
			}},
		},
		"provider": obj{"aws": obj{
			"region":       c.Region,
			"default_tags": obj{"tags": obj{"bbctl:deployment": c.Name}},
		}},
	}

	var mod obj
	var outputs []string
	switch s.Kind {
	case KindNetwork:
		mod, outputs = networkModule(c, in)
	case KindPool:
		p, _ := c.Pool(s.Pool)
		mod, outputs = poolModule(c, p)
		doc["data"] = networkRemoteState(c, all[0])
	case KindMgmt:
		mod, outputs = mgmtModule(c)
		doc["data"] = networkRemoteState(c, all[0])
	}
	mod["source"] = src
	name := string(s.Kind)
	doc["module"] = obj{name: mod}

	out := obj{}
	for _, o := range outputs {
		out[o] = obj{"value": fmt.Sprintf("${module.%s.%s}", name, o)}
	}
	doc["output"] = out
	return doc, nil
}

// moduleSource is the module path relative to the stack directory: Terraform
// only treats ./ and ../ paths as local modules.
func moduleSource(stackDir, modulesDir, name string) (string, error) {
	absStack, err := filepath.Abs(stackDir)
	if err != nil {
		return "", err
	}
	absMod, err := filepath.Abs(filepath.Join(modulesDir, "modules", name))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absStack, absMod)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if rel[0] != '.' {
		rel = "./" + rel
	}
	return rel, nil
}

func networkRemoteState(c *config.Config, network Stack) obj {
	return obj{"terraform_remote_state": obj{"network": obj{
		"backend": "s3",
		"config": obj{
			"bucket": c.State.Bucket,
			"key":    network.StateKey,
			"region": c.Region,
		},
	}}}
}

const networkRef = "${data.terraform_remote_state.network.outputs}"

func networkModule(c *config.Config, in Inputs) (obj, []string) {
	n := c.Network
	bastion := obj{"enable": false, "vpc_subnet": "", "ami": "", "instance_type": "t4g.micro", "whitelist_ips": []string{}}
	if b := n.Bastion; b != nil && b.Enable {
		bastion = obj{
			"enable":        true,
			"vpc_subnet":    n.NATPublicSubnetID,
			"ami":           b.AMI,
			"instance_type": orDefault(b.InstanceType, "t4g.micro"),
			"whitelist_ips": nonNil(b.WhitelistIPs),
		}
	}
	return obj{
			"name_prefix":          c.Name,
			"vpc_id":               n.VPCID,
			"azs":                  n.AZs,
			"mgmt_subnet_cidrs":    n.MgmtSubnetCIDRs,
			"storage_subnet_cidrs": n.StorageSubnetCIDRs,
			"nat_public_subnet_id": n.NATPublicSubnetID,
			"bastion":              bastion,
			"ssh_public_key_path":  in.SSHPublicKeyPath,
			"key_name":             c.Name + "-deployer",
		}, []string{
			"azs", "mgmt_subnet_ids", "storage_subnet_ids", "mgmt_subnet_cidrs",
			"storage_subnet_cidrs", "internal_sg_id", "key_name",
			"bastion_enabled", "bastion_public_ip", "bastion_private_ip",
		}
}

func poolModule(c *config.Config, p *config.Pool) (obj, []string) {
	vols := make([]obj, 0, len(p.EBSVolumes))
	for _, v := range p.EBSVolumes {
		vols = append(vols, obj{"size": v.Size, "type": v.Type, "iops": v.IOPS, "throughput": v.Throughput, "count": v.Count})
	}
	m := obj{
		"cluster":                c.Cluster,
		"pool_name":              p.Name,
		"region":                 c.Region,
		"network":                networkRef,
		"az":                     p.AZ,
		"description":            orDefault(p.Description, "Pool "+p.Name),
		"labels":                 orDefault(p.Labels, "name="+p.Name),
		"nodes_ami":              c.NodesAMI,
		"nodes_instance_type":    p.InstanceType,
		"nodes_count":            p.NodesCount,
		"raid_level":             p.RaidLevel,
		"nvme_node_disks_count":  p.DiskCount(),
		"max_volumes_count":      p.MaxVolumesCount,
		"r_cache_size_in_mib":    p.RCacheSizeInMiB,
		"rw_cache_size_in_mib":   p.RWCacheSizeInMiB,
		"ebs_volumes":            vols,
		"s3_bucket_names":        p.S3BucketNames,
		"s3_backup_bucket_names": p.S3BackupBuckets,
		"s3_bucket_access_names": nonNil(p.S3BucketAccess),
		"s3_force_destroy":       p.S3ForceDestroy,
		"enable_metrics":         p.EnableMetrics,
		// A standalone pool scrapes every peer for a full metrics replica; under
		// a mgmt plane each node scrapes itself and mgmt federates them.
		"cross_peer_scrape": c.Mgmt == nil,
		"provision_mode":    "ssm",
		"secrets_ssm_path":  c.SecretsSSMPath,
	}
	for k, v := range p.Extra {
		m[k] = v
	}
	return m, []string{
		"pool_name", "node_mgmt_private_ips", "node_data_private_ips", "instance_ids",
		"ssm_registry_parameter", "s3_bucket_names", "s3_backup_bucket_names",
	}
}

func mgmtModule(c *config.Config) (obj, []string) {
	g := c.Mgmt
	m := obj{
		"cluster":             c.Cluster,
		"region":              c.Region,
		"network":             networkRef,
		"nodes_ami":           c.NodesAMI,
		"nodes_instance_type": g.InstanceType,
		"nodes_count":         g.NodesCount,
		"enable_metrics":      g.EnableMetrics,
		"provision_mode":      "ssm",
		"secrets_ssm_path":    c.SecretsSSMPath,
	}
	for k, v := range g.Extra {
		m[k] = v
	}
	return m, []string{"node_private_ips", "instance_ids", "discovered_pools"}
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
