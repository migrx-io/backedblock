package awsx

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// SecretKeys are the keys every node reads from secrets.env, in file order.
var SecretKeys = []string{
	"CASS_USER", "CASS_PASSWD",
	"MGX_GW_X_API_KEY", "MGX_X_API_KEY",
	"MGX_GW_ADMIN_PASSWD",
	"GRAFANA_USER", "GRAFANA_PASSWD",
}

var fixedSecrets = map[string]string{"CASS_USER": "mgx", "GRAFANA_USER": "admin"}

// GenerateSecrets returns random values for every key; user names are fixed.
func GenerateSecrets() (map[string]string, error) {
	s := map[string]string{}
	for _, k := range SecretKeys {
		if v, ok := fixedSecrets[k]; ok {
			s[k] = v
			continue
		}
		v, err := randomString(32)
		if err != nil {
			return nil, err
		}
		s[k] = v
	}
	return s, nil
}

func randomString(n int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		x, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[x.Int64()]
	}
	return string(b), nil
}

// FormatSecrets renders the secrets.env content nodes expect.
func FormatSecrets(s map[string]string) string {
	var b strings.Builder
	for _, k := range SecretKeys {
		fmt.Fprintf(&b, "%s=%q\n", k, s[k])
	}
	return b.String()
}

// ParseSecrets reads KEY="value" lines, ignoring comments and blanks.
func ParseSecrets(content string) map[string]string {
	s := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(content))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		s[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return s
}

// MissingSecrets lists required keys absent or left as placeholders.
func MissingSecrets(s map[string]string) []string {
	var miss []string
	for _, k := range SecretKeys {
		if v := s[k]; v == "" || strings.HasPrefix(v, "<") {
			miss = append(miss, k)
		}
	}
	return miss
}

// GetSecrets reads the SecureString. found is false if it does not exist.
func (c *Client) GetSecrets(ctx context.Context, path string) (s map[string]string, found bool, err error) {
	out, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(path), WithDecryption: aws.Bool(true)})
	if err != nil {
		var nf *types.ParameterNotFound
		if errors.As(err, &nf) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return ParseSecrets(aws.ToString(out.Parameter.Value)), true, nil
}

// PutSecrets writes the SecureString, replacing any existing value.
func (c *Client) PutSecrets(ctx context.Context, path string, s map[string]string) error {
	_, err := c.SSM.PutParameter(ctx, &ssm.PutParameterInput{
		Name:      aws.String(path),
		Type:      types.ParameterTypeSecureString,
		Value:     aws.String(FormatSecrets(s)),
		Overwrite: aws.Bool(true),
		Tier:      types.ParameterTierStandard,
	})
	return err
}

// DeleteSecrets deletes the SecureString. It reports whether it existed; a
// missing one is not an error.
func (c *Client) DeleteSecrets(ctx context.Context, path string) (bool, error) {
	_, err := c.SSM.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(path)})
	if err != nil {
		var nf *types.ParameterNotFound
		if errors.As(err, &nf) {
			return false, nil
		}
		return false, fmt.Errorf("delete %s: %w", path, err)
	}
	return true, nil
}
