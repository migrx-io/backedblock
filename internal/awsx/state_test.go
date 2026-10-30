package awsx

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func respErr(code int) error {
	return fmt.Errorf("operation error S3: HeadBucket: %w", &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}},
			Err:      errors.New("api error"),
		},
	})
}

func TestIsNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"404", respErr(404), true},
		{"typed NotFound", &types.NotFound{}, true},
		{"typed NoSuchKey", &types.NoSuchKey{}, true},
		{"403 is not 'missing'", respErr(403), false},
		{"500 is not 'missing'", respErr(500), false},
		{"network error is not 'missing'", errors.New("dial tcp: i/o timeout"), false},
	}
	for _, tt := range tests {
		if got := isNotFound(tt.err); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
	if httpStatus(respErr(403)) != 403 {
		t.Error("httpStatus did not unwrap the response error")
	}
}
