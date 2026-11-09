// Package config is the bbctl.yaml schema: one file describes a whole
// deployment (network, optional management plane, storage pools) and is the
// only thing a user or agent edits. Everything else is rendered from it.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultFile is the config file bbctl looks for in the working directory.
const DefaultFile = "bbctl.yaml"

type Config struct {
	// Name identifies the deployment: it prefixes AWS resource names and keys
	// the Terraform state, so several deployments can share one account.
	Name    string `yaml:"name"`
	Region  string `yaml:"region"`
	Cluster string `yaml:"cluster"`

	State     State     `yaml:"state"`
	Modules   Modules   `yaml:"modules"`
	Terraform Terraform `yaml:"terraform,omitempty"`

	NodesAMI       string `yaml:"nodes_ami"`
	SecretsSSMPath string `yaml:"secrets_ssm_path"`

	Network Network `yaml:"network"`
	// Mgmt is the management plane. Leave it out for a standalone single pool.
	Mgmt  *Mgmt  `yaml:"mgmt,omitempty"`
	Pools []Pool `yaml:"pools"`
}

type State struct {
	// Bucket holds the Terraform state of every stack. bbctl creates it.
	Bucket string `yaml:"bucket"`
}

type Modules struct {
	// Ref is the git ref of github.com/migrx-io/terraform-aws-mgx to deploy.
	Ref string `yaml:"ref"`
	// Path, when set, uses a local checkout of the modules repo instead.
	Path string `yaml:"path,omitempty"`
}

type Terraform struct {
	// Version of Terraform bbctl downloads and runs. Empty means the pinned default.
	Version string `yaml:"version,omitempty"`
	// Path to an existing terraform binary; skips the download.
	Path string `yaml:"path,omitempty"`
}

type Network struct {
	VPCID              string   `yaml:"vpc_id"`
	AZs                []string `yaml:"azs"`
	MgmtSubnetCIDRs    []string `yaml:"mgmt_subnet_cidrs"`
	StorageSubnetCIDRs []string `yaml:"storage_subnet_cidrs"`
	// NATPublicSubnetID is a public subnet (route to an internet gateway) for
	// the NAT gateway the nodes use to reach SSM and S3.
	NATPublicSubnetID string `yaml:"nat_public_subnet_id"`
	// SSHPublicKeyPath registers an EC2 key pair. Empty means bbctl generates
	// one under .bbctl/ssh (nodes are reached through SSM, not SSH, by default).
	SSHPublicKeyPath string   `yaml:"ssh_public_key_path,omitempty"`
	Bastion          *Bastion `yaml:"bastion,omitempty"`
}

type Bastion struct {
	Enable       bool     `yaml:"enable"`
	AMI          string   `yaml:"ami"`
	InstanceType string   `yaml:"instance_type"`
	WhitelistIPs []string `yaml:"whitelist_ips"`
}

type Mgmt struct {
	NodesCount    int    `yaml:"nodes_count"`
	InstanceType  string `yaml:"instance_type"`
	EnableMetrics bool   `yaml:"enable_metrics"`
	// Extra is passed through to the mgmt module as-is (any module variable).
	Extra map[string]any `yaml:"extra,omitempty"`
}

type Pool struct {
	Name        string `yaml:"name"`
	AZ          string `yaml:"az"`
	Description string `yaml:"description,omitempty"`
	Labels      string `yaml:"labels,omitempty"`

	NodesCount   int    `yaml:"nodes_count"`
	InstanceType string `yaml:"instance_type"`

	RaidLevel        int         `yaml:"raid_level"`
	MaxVolumesCount  int         `yaml:"max_volumes_count"`
	RCacheSizeInMiB  int         `yaml:"r_cache_size_in_mib"`
	RWCacheSizeInMiB int         `yaml:"rw_cache_size_in_mib"`
	EBSVolumes       []EBSVolume `yaml:"ebs_volumes"`
	S3BucketNames    []string    `yaml:"s3_bucket_names"`
	S3BackupBuckets  []string    `yaml:"s3_backup_bucket_names"`
	S3BucketAccess   []string    `yaml:"s3_bucket_access_names,omitempty"`
	S3ForceDestroy   bool        `yaml:"s3_force_destroy"`
	EnableMetrics    bool        `yaml:"enable_metrics"`

	// StorageClass is the parameters of the pool's Kubernetes StorageClass,
	// e.g. qos_rw_ios_per_sec or max_cache_r_cache_size, as the CSI driver
	// names them. config create writes the calculator's; bbctl csi passes
	// them to the driver's chart. Not used by the modules.
	StorageClass map[string]string `yaml:"storage_class,omitempty"`

	// Extra is passed through to the pool module as-is, e.g. cache_filesystem,
	// scheduler_node_down_wait, qos_* or snapshot_* settings.
	Extra map[string]any `yaml:"extra,omitempty"`
}

type EBSVolume struct {
	Size       int    `yaml:"size"`
	Type       string `yaml:"type"`
	IOPS       int    `yaml:"iops"`
	Throughput int    `yaml:"throughput"`
	Count      int    `yaml:"count"`
}

// DiskCount is the total number of EBS cache volumes on each node.
func (p Pool) DiskCount() int {
	n := 0
	for _, v := range p.EBSVolumes {
		n += v.Count
	}
	return n
}

// Pool returns the pool with the given name.
func (c *Config) Pool(name string) (*Pool, bool) {
	for i := range c.Pools {
		if c.Pools[i].Name == name {
			return &c.Pools[i], true
		}
	}
	return nil, false
}

// ErrNotFound is returned by Load when the config file does not exist.
var ErrNotFound = errors.New("config file not found")

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

const header = `# bbctl deployment config. Edit, then run: bbctl plan / bbctl apply
# Reference: https://backedblock.io/docs
`

// Save writes the config atomically: readers see the old file or the new
// one, never a partial write.
func (c *Config) Save(path string) error {
	st, err := c.Stage(path)
	if err != nil {
		return err
	}
	return st.Commit()
}

// Staged is a config fully written to a temp file next to its destination,
// waiting to be renamed into place. Staging before an irreversible step
// (e.g. destroying a pool) proves the config can be written, leaving only an
// atomic same-directory rename for afterwards.
type Staged struct {
	tmp, path string
}

func (c *Config) Stage(path string) (*Staged, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", path, err)
	}
	_, err = f.Write(append([]byte(header), b...))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err != nil {
		os.Remove(f.Name())
		return nil, fmt.Errorf("cannot write %s: %w", path, err)
	}
	return &Staged{tmp: f.Name(), path: path}, nil
}

// Commit renames the staged file into place.
func (s *Staged) Commit() error {
	if err := os.Rename(s.tmp, s.path); err != nil {
		return fmt.Errorf("cannot replace %s: %w", s.path, err)
	}
	return nil
}

// Discard removes the staged file. Safe to call after Commit.
func (s *Staged) Discard() { os.Remove(s.tmp) }

var (
	nameRe   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	poolRe   = regexp.MustCompile(`^[a-zA-Z0-9-]+$`)
	bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
)

// IsPlaceholder reports whether s is a placeholder such as <vpc id>, written by
// config create where its credentials could not look the value up.
func IsPlaceholder(s string) bool {
	return len(s) > 2 && strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">")
}

// Placeholders lists the fields that still hold a placeholder, as
// "field: <value>".
func (c *Config) Placeholders() []string {
	var ph []string
	check := func(field, v string) {
		if IsPlaceholder(v) {
			ph = append(ph, field+": "+v)
		}
	}
	checkAll := func(field string, vs []string) {
		for i, v := range vs {
			check(fmt.Sprintf("%s[%d]", field, i), v)
		}
	}
	check("nodes_ami", c.NodesAMI)
	check("network.vpc_id", c.Network.VPCID)
	checkAll("network.azs", c.Network.AZs)
	checkAll("network.mgmt_subnet_cidrs", c.Network.MgmtSubnetCIDRs)
	checkAll("network.storage_subnet_cidrs", c.Network.StorageSubnetCIDRs)
	check("network.nat_public_subnet_id", c.Network.NATPublicSubnetID)
	for i, p := range c.Pools {
		check(fmt.Sprintf("pools[%d].az", i), p.AZ)
	}
	return ph
}

// Validate returns every problem found, not just the first, so one run tells
// the caller everything to fix. Placeholders count as problems: Terraform
// would fail on them.
func (c *Config) Validate() []string {
	errs := c.ValidateDraft()
	for _, p := range c.Placeholders() {
		errs = append(errs, p+" is a placeholder; replace it with the real value")
	}
	return errs
}

// ValidateDraft is Validate without the placeholder check, for config create,
// which writes placeholders for what it could not look up.
func (c *Config) ValidateDraft() []string {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	if !nameRe.MatchString(c.Name) {
		add("name: %q must be lowercase letters, digits and dashes, starting with a letter (max 31)", c.Name)
	}
	if c.Region == "" {
		add("region: required")
	}
	if c.Cluster == "" {
		add("cluster: required")
	}
	if c.State.Bucket == "" {
		add("state.bucket: required")
	}
	if c.Modules.Ref == "" && c.Modules.Path == "" {
		add("modules: set ref or path")
	}
	if c.NodesAMI == "" {
		add("nodes_ami: required (see https://backedblock.io/docs/node-amis)")
	}
	if !strings.HasPrefix(c.SecretsSSMPath, "/") {
		add("secrets_ssm_path: must be an absolute SSM parameter path, e.g. /mgx/main/secrets")
	}

	n := c.Network
	if n.VPCID == "" {
		add("network.vpc_id: required")
	}
	if len(n.AZs) == 0 {
		add("network.azs: at least one AZ is required")
	}
	if len(n.MgmtSubnetCIDRs) != len(n.AZs) {
		add("network.mgmt_subnet_cidrs: need one CIDR per AZ (%d AZs, %d CIDRs)", len(n.AZs), len(n.MgmtSubnetCIDRs))
	}
	if len(n.StorageSubnetCIDRs) != len(n.AZs) {
		add("network.storage_subnet_cidrs: need one CIDR per AZ (%d AZs, %d CIDRs)", len(n.AZs), len(n.StorageSubnetCIDRs))
	}
	if n.NATPublicSubnetID == "" {
		add("network.nat_public_subnet_id: required (a public subnet for the NAT gateway)")
	}
	if b := n.Bastion; b != nil && b.Enable && b.AMI == "" {
		add("network.bastion.ami: required when the bastion is enabled")
	}

	if c.Mgmt != nil && c.Mgmt.NodesCount < 1 {
		add("mgmt.nodes_count: must be >= 1")
	}

	if len(c.Pools) == 0 {
		add("pools: at least one pool is required")
	}
	seen := map[string]bool{}
	buckets := map[string]string{}
	for i, p := range c.Pools {
		at := fmt.Sprintf("pools[%d]", i)
		if !poolRe.MatchString(p.Name) {
			add("%s.name: %q must be letters, digits and dashes", at, p.Name)
		}
		if seen[p.Name] {
			add("%s.name: duplicate pool %q", at, p.Name)
		}
		seen[p.Name] = true
		if !contains(n.AZs, p.AZ) {
			add("%s.az: %q is not one of network.azs %v", at, p.AZ, n.AZs)
		}
		if p.NodesCount < 1 {
			add("%s.nodes_count: must be >= 1", at)
		}
		if p.InstanceType == "" {
			add("%s.instance_type: required", at)
		}
		if len(p.EBSVolumes) == 0 {
			add("%s.ebs_volumes: at least one cache volume group is required", at)
		}
		if len(p.S3BucketNames) == 0 || len(p.S3BackupBuckets) == 0 {
			add("%s: s3_bucket_names and s3_backup_bucket_names are required", at)
		}
		for _, b := range append(append([]string{}, p.S3BucketNames...), p.S3BackupBuckets...) {
			if !bucketRe.MatchString(b) {
				add("%s: %q is not a valid S3 bucket name", at, b)
			}
			if other, ok := buckets[b]; ok {
				add("%s: bucket %q is already used by pool %q", at, b, other)
			}
			buckets[b] = p.Name
		}
	}
	return errs
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
