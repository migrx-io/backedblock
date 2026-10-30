package awsx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session"
	_ "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/portsession"
	_ "github.com/aws/session-manager-plugin/src/sessionmanagerplugin/session/shellsession"
)

// Session opens an interactive shell (localPort == 0) or forwards localPort
// to remotePort on the instance.
//
// The session protocol runs in-process: bbctl links AWS's session-manager-
// plugin code and calls its entry point with the same arguments the aws CLI
// passes to the plugin binary, so shells, multiplexed port forwarding and
// session resume behave exactly as with the plugin, without installing it.
//
// On success it does not return: the plugin ends the process (exit 0) when
// the session closes, so callers must have nothing left to do.
func (c *Client) Session(ctx context.Context, id string, remotePort, localPort int, profile string) error {
	in := &ssm.StartSessionInput{Target: aws.String(id)}
	if localPort > 0 {
		in.DocumentName = aws.String("AWS-StartPortForwardingSession")
		in.Parameters = map[string][]string{
			"portNumber":      {fmt.Sprint(remotePort)},
			"localPortNumber": {fmt.Sprint(localPort)},
		}
	}
	endpoint, err := ssm.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, ssm.EndpointParameters{Region: aws.String(c.Region())})
	if err != nil {
		return fmt.Errorf("resolve the SSM endpoint for %s: %w", c.Region(), err)
	}
	out, err := c.SSM.StartSession(ctx, in)
	if err != nil {
		return err
	}
	resp, err := json.Marshal(map[string]string{
		"SessionId":  aws.ToString(out.SessionId),
		"TokenValue": aws.ToString(out.TokenValue),
		"StreamUrl":  aws.ToString(out.StreamUrl),
	})
	if err != nil {
		return err
	}
	params, err := json.Marshal(in)
	if err != nil {
		return err
	}

	// The plugin reports failures as text on this writer and returns; the
	// success path never comes back here.
	var msg bytes.Buffer
	session.ValidateInputAndStartSession([]string{
		"session-manager-plugin", string(resp), c.Region(), "StartSession", profile, string(params), endpoint.URI.String(),
	}, io.MultiWriter(os.Stderr, &msg))

	if m := strings.TrimSpace(msg.String()); m != "" {
		return errors.New(m)
	}
	return fmt.Errorf("session %s to %s ended unexpectedly", aws.ToString(out.SessionId), id)
}
