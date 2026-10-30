package awsx

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// EnsureStateBucket creates the Terraform state bucket if it does not exist,
// with versioning on and public access blocked. Returns true if it was created.
func (c *Client) EnsureStateBucket(ctx context.Context, bucket string) (bool, error) {
	exists, err := c.StateBucketExists(ctx, bucket)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if c.Region() != "us-east-1" {
		in.CreateBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(c.Region()),
		}
	}
	if _, err := c.S3.CreateBucket(ctx, in); err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		if !errors.As(err, &owned) {
			return false, fmt.Errorf("create state bucket %s: %w", bucket, err)
		}
	}
	if _, err := c.S3.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled},
	}); err != nil {
		return true, fmt.Errorf("enable versioning on %s: %w", bucket, err)
	}
	if _, err := c.S3.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucket),
		PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{
			BlockPublicAcls: aws.Bool(true), BlockPublicPolicy: aws.Bool(true),
			IgnorePublicAcls: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true),
		},
	}); err != nil {
		return true, fmt.Errorf("block public access on %s: %w", bucket, err)
	}
	return true, nil
}

// StateBucketExists reports whether the state bucket exists and is ours.
// Only a 404 means "does not exist"; any other failure (denied, owned by
// another account, network) is returned as an error so it is never mistaken
// for a fresh deployment.
func (c *Client) StateBucketExists(ctx context.Context, bucket string) (bool, error) {
	_, err := c.S3.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	switch {
	case err == nil:
		return true, nil
	case isNotFound(err):
		return false, nil
	case httpStatus(err) == http.StatusForbidden:
		return false, fmt.Errorf("state bucket s3://%s: access denied, or the name is taken by another account (needs s3:ListBucket; else pick another state.bucket)", bucket)
	case httpStatus(err) == http.StatusMovedPermanently:
		return false, fmt.Errorf("state bucket s3://%s exists in another region than %s, possibly in another account; pick another state.bucket", bucket, c.Region())
	}
	return false, fmt.Errorf("state bucket s3://%s: %w", bucket, err)
}

// StateExists reports whether a stack has state in the bucket, i.e. was
// applied. As with StateBucketExists, only a 404 means "no".
func (c *Client) StateExists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := c.S3.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	switch {
	case err == nil:
		return true, nil
	case isNotFound(err):
		return false, nil
	}
	return false, fmt.Errorf("read state s3://%s/%s: %w", bucket, key, err)
}

func isNotFound(err error) bool {
	var nf *types.NotFound
	var nsb *types.NoSuchBucket
	var nsk *types.NoSuchKey
	return errors.As(err, &nf) || errors.As(err, &nsb) || errors.As(err, &nsk) || httpStatus(err) == http.StatusNotFound
}

func httpStatus(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}
