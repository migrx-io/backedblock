package awsx

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type VPC struct {
	ID        string   `json:"id"`
	Name      string   `json:"name,omitempty"`
	CIDRs     []string `json:"cidrs"`
	IsDefault bool     `json:"default"`
}

func (c *Client) VPCs(ctx context.Context) ([]VPC, error) {
	out, err := c.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		return nil, err
	}
	var vs []VPC
	for _, v := range out.Vpcs {
		vpc := VPC{ID: aws.ToString(v.VpcId), Name: tagName(v.Tags), IsDefault: aws.ToBool(v.IsDefault)}
		for _, a := range v.CidrBlockAssociationSet {
			if a.CidrBlockState != nil && a.CidrBlockState.State == types.VpcCidrBlockStateCodeAssociated {
				vpc.CIDRs = append(vpc.CIDRs, aws.ToString(a.CidrBlock))
			}
		}
		vs = append(vs, vpc)
	}
	return vs, nil
}

// AZsOffering returns the region's standard AZs (no local/wavelength zones)
// where every given instance type is offered, in name order.
func (c *Client) AZsOffering(ctx context.Context, instanceTypes ...string) ([]string, error) {
	azs, err := c.EC2.DescribeAvailabilityZones(ctx, &ec2.DescribeAvailabilityZonesInput{
		Filters: []types.Filter{
			{Name: aws.String("zone-type"), Values: []string{"availability-zone"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return nil, err
	}
	ok := map[string]int{}
	for _, it := range instanceTypes {
		p := ec2.NewDescribeInstanceTypeOfferingsPaginator(c.EC2, &ec2.DescribeInstanceTypeOfferingsInput{
			LocationType: types.LocationTypeAvailabilityZone,
			Filters:      []types.Filter{{Name: aws.String("instance-type"), Values: []string{it}}},
		})
		for p.HasMorePages() {
			page, err := p.NextPage(ctx)
			if err != nil {
				return nil, err
			}
			for _, o := range page.InstanceTypeOfferings {
				ok[aws.ToString(o.Location)]++
			}
		}
	}
	var res []string
	for _, z := range azs.AvailabilityZones {
		name := aws.ToString(z.ZoneName)
		if ok[name] == len(instanceTypes) {
			res = append(res, name)
		}
	}
	sort.Strings(res)
	return res, nil
}

type Subnet struct {
	ID     string `json:"id"`
	AZ     string `json:"az"`
	CIDR   string `json:"cidr"`
	Name   string `json:"name,omitempty"`
	Public bool   `json:"public"`
}

// Subnets lists a VPC's subnets, marking as public those whose effective route
// table (explicit association, else the VPC's main table) has an active
// 0.0.0.0/0 route to an internet gateway. A blackhole route, e.g. after the
// gateway was detached, does not count.
func (c *Client) Subnets(ctx context.Context, vpcID string) ([]Subnet, error) {
	vpcFilter := []types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}}
	sn, err := c.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{Filters: vpcFilter})
	if err != nil {
		return nil, err
	}
	rts, err := c.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{Filters: vpcFilter})
	if err != nil {
		return nil, err
	}
	explicit := map[string]bool{} // subnet -> public
	mainPublic := false
	for _, rt := range rts.RouteTables {
		pub := routesToIGW(rt)
		for _, a := range rt.Associations {
			if aws.ToBool(a.Main) {
				mainPublic = pub
			} else if a.SubnetId != nil {
				explicit[*a.SubnetId] = pub
			}
		}
	}
	var res []Subnet
	for _, s := range sn.Subnets {
		id := aws.ToString(s.SubnetId)
		pub, ok := explicit[id]
		if !ok {
			pub = mainPublic
		}
		res = append(res, Subnet{
			ID: id, AZ: aws.ToString(s.AvailabilityZone), CIDR: aws.ToString(s.CidrBlock),
			Name: tagName(s.Tags), Public: pub,
		})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].AZ+res[i].CIDR < res[j].AZ+res[j].CIDR })
	return res, nil
}

// routesToIGW reports whether a route table has an active default route to
// an internet gateway.
func routesToIGW(rt types.RouteTable) bool {
	for _, r := range rt.Routes {
		if aws.ToString(r.DestinationCidrBlock) == "0.0.0.0/0" &&
			strings.HasPrefix(aws.ToString(r.GatewayId), "igw-") &&
			r.State == types.RouteStateActive {
			return true
		}
	}
	return false
}

// FreeCIDRs carves n non-overlapping blocks out of the VPC's CIDRs that do not
// overlap any existing subnet. It tries /20 first and shrinks, down to /24, if
// the VPC is too small or too full.
func FreeCIDRs(vpcCIDRs []string, used []string, n int) ([]string, error) {
	var taken []netip.Prefix
	for _, u := range used {
		if p, err := netip.ParsePrefix(u); err == nil {
			taken = append(taken, p)
		}
	}
	for bits := 20; bits <= 24; bits++ {
		var got []string
		t := append([]netip.Prefix{}, taken...)
		for _, vc := range vpcCIDRs {
			vp, err := netip.ParsePrefix(vc)
			if err != nil || !vp.Addr().Is4() || vp.Bits() > bits {
				continue
			}
			size := uint32(1) << (32 - bits)
			start := v4(vp.Masked().Addr())
			end := start + (uint32(1) << (32 - vp.Bits()))
			for a := start; a < end && len(got) < n; a += size {
				cand := netip.PrefixFrom(fromV4(a), bits)
				if !overlapsAny(cand, t) {
					got = append(got, cand.String())
					t = append(t, cand)
				}
			}
		}
		if len(got) == n {
			return got, nil
		}
	}
	return nil, fmt.Errorf("VPC %v has no room for %d free subnets of /24 or larger", vpcCIDRs, n)
}

func overlapsAny(p netip.Prefix, ps []netip.Prefix) bool {
	for _, q := range ps {
		if p.Overlaps(q) {
			return true
		}
	}
	return false
}

func v4(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func fromV4(u uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)})
}

// Arch returns the CPU architecture (arm64 or x86_64) of an instance type.
func (c *Client) Arch(ctx context.Context, instanceType string) (string, error) {
	out, err := c.EC2.DescribeInstanceTypes(ctx, &ec2.DescribeInstanceTypesInput{
		InstanceTypes: []types.InstanceType{types.InstanceType(instanceType)},
	})
	if err != nil {
		return "", err
	}
	if len(out.InstanceTypes) == 0 || out.InstanceTypes[0].ProcessorInfo == nil {
		return "", fmt.Errorf("unknown instance type %s", instanceType)
	}
	for _, a := range out.InstanceTypes[0].ProcessorInfo.SupportedArchitectures {
		if a == types.ArchitectureTypeArm64 || a == types.ArchitectureTypeX8664 {
			return string(a), nil
		}
	}
	return "", fmt.Errorf("instance type %s: unsupported architecture", instanceType)
}

// AMIOwner is the account that publishes the prebaked node images.
const AMIOwner = "222757277183"

type Image struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Arch    string `json:"arch"`
	Created string `json:"created"`
}

// LatestNodeAMI finds the newest published node image for an architecture.
func (c *Client) LatestNodeAMI(ctx context.Context, arch string) (Image, error) {
	out, err := c.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{
		Owners: []string{AMIOwner},
		Filters: []types.Filter{
			{Name: aws.String("name"), Values: []string{"mgx-storage-*-" + arch + "-*"}},
			{Name: aws.String("state"), Values: []string{"available"}},
		},
	})
	if err != nil {
		return Image{}, err
	}
	if len(out.Images) == 0 {
		return Image{}, fmt.Errorf("no %s node AMI visible in %s", arch, c.Region())
	}
	sort.Slice(out.Images, func(i, j int) bool {
		return aws.ToString(out.Images[i].CreationDate) > aws.ToString(out.Images[j].CreationDate)
	})
	im := out.Images[0]
	return Image{ID: aws.ToString(im.ImageId), Name: aws.ToString(im.Name), Arch: string(im.Architecture), Created: aws.ToString(im.CreationDate)}, nil
}

// Image describes one AMI, or errors if it is not visible to the account.
func (c *Client) Image(ctx context.Context, id string) (Image, error) {
	out, err := c.EC2.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{id}})
	if err != nil {
		return Image{}, err
	}
	if len(out.Images) == 0 {
		return Image{}, fmt.Errorf("AMI %s not found in %s", id, c.Region())
	}
	im := out.Images[0]
	return Image{ID: id, Name: aws.ToString(im.Name), Arch: string(im.Architecture), Created: aws.ToString(im.CreationDate)}, nil
}

func tagName(tags []types.Tag) string {
	for _, t := range tags {
		if aws.ToString(t.Key) == "Name" {
			return aws.ToString(t.Value)
		}
	}
	return ""
}
