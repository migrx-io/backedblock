package awsx

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

func TestFreeCIDRs(t *testing.T) {
	tests := []struct {
		name string
		vpc  []string
		used []string
		n    int
		want []string
	}{
		{
			name: "default VPC skips the default subnets",
			vpc:  []string{"172.31.0.0/16"},
			used: []string{"172.31.0.0/20", "172.31.16.0/20", "172.31.32.0/20", "172.31.48.0/20", "172.31.64.0/20", "172.31.80.0/20"},
			n:    6,
			want: []string{"172.31.96.0/20", "172.31.112.0/20", "172.31.128.0/20", "172.31.144.0/20", "172.31.160.0/20", "172.31.176.0/20"},
		},
		{
			name: "skips blocks overlapping a smaller subnet",
			vpc:  []string{"10.0.0.0/16"},
			used: []string{"10.0.5.0/24"},
			n:    2,
			want: []string{"10.0.16.0/20", "10.0.32.0/20"},
		},
		{
			name: "small VPC shrinks to the largest size that fits",
			vpc:  []string{"10.1.0.0/22"},
			n:    2,
			want: []string{"10.1.0.0/23", "10.1.2.0/23"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FreeCIDRs(tt.vpc, tt.used, tt.n)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
	if _, err := FreeCIDRs([]string{"10.2.0.0/24"}, nil, 2); err == nil {
		t.Fatal("expected an error for a VPC with no room")
	}
}

func TestSecretsRoundTrip(t *testing.T) {
	s, err := GenerateSecrets()
	if err != nil {
		t.Fatal(err)
	}
	got := ParseSecrets("# comment\n" + FormatSecrets(s))
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("round trip mismatch: %v vs %v", got, s)
	}
	if m := MissingSecrets(got); len(m) != 0 {
		t.Fatalf("missing %v", m)
	}
	if m := MissingSecrets(ParseSecrets(`CASS_USER="<CASS_USER>"`)); len(m) != len(SecretKeys) {
		t.Fatalf("placeholders should count as missing, got %v", m)
	}
}

func TestRoutesToIGW(t *testing.T) {
	route := func(dst, gw string, st types.RouteState) types.Route {
		return types.Route{DestinationCidrBlock: aws.String(dst), GatewayId: aws.String(gw), State: st}
	}
	tests := []struct {
		name string
		rt   types.RouteTable
		want bool
	}{
		{"active default route to igw", types.RouteTable{Routes: []types.Route{route("0.0.0.0/0", "igw-1", types.RouteStateActive)}}, true},
		{"blackhole after igw detached", types.RouteTable{Routes: []types.Route{route("0.0.0.0/0", "igw-1", types.RouteStateBlackhole)}}, false},
		{"default route to a NAT, not an igw", types.RouteTable{Routes: []types.Route{{DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: aws.String("nat-1"), State: types.RouteStateActive}}}, false},
		{"igw but not the default route", types.RouteTable{Routes: []types.Route{route("10.9.0.0/16", "igw-1", types.RouteStateActive)}}, false},
		{"local only", types.RouteTable{Routes: []types.Route{route("172.31.0.0/16", "local", types.RouteStateActive)}}, false},
	}
	for _, tt := range tests {
		if got := routesToIGW(tt.rt); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}
