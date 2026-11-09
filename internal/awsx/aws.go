// Package awsx is everything bbctl asks AWS directly, through the SDK rather
// than the aws CLI: identity, discovery of the values bbctl.yaml needs, the
// state bucket, node secrets and node access over SSM.
package awsx

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
)

// IsDenied reports whether AWS refused the call for lack of permission, as
// opposed to a network or throttling error.
func IsDenied(err error) bool {
	var ae smithy.APIError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.ErrorCode() {
	case "UnauthorizedOperation", "AccessDenied", "AccessDeniedException":
		return true
	}
	return false
}

type Client struct {
	Cfg aws.Config
	EC2 *ec2.Client
	SSM *ssm.Client
	S3  *s3.Client
	STS *sts.Client
}

// New loads credentials from the standard chain (env, ~/.aws, SSO, instance
// role). region may be empty to use the profile's default.
func New(ctx context.Context, region, profile string) (*Client, error) {
	var opts []func(*config.LoadOptions) error
	if region != "" {
		opts = append(opts, config.WithRegion(region))
	}
	if profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if cfg.Region == "" {
		return nil, errors.New("no AWS region: pass --region or set AWS_REGION")
	}
	return &Client{
		Cfg: cfg,
		EC2: ec2.NewFromConfig(cfg),
		SSM: ssm.NewFromConfig(cfg),
		S3:  s3.NewFromConfig(cfg),
		STS: sts.NewFromConfig(cfg),
	}, nil
}

func (c *Client) Region() string { return c.Cfg.Region }

type Identity struct {
	Account string `json:"account"`
	ARN     string `json:"arn"`
}

func (c *Client) Identity(ctx context.Context) (Identity, error) {
	out, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return Identity{}, fmt.Errorf("AWS credentials: %w", err)
	}
	return Identity{Account: aws.ToString(out.Account), ARN: aws.ToString(out.Arn)}, nil
}
