package awsx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type Node struct {
	InstanceID string `json:"instance_id"`
	State      string `json:"state"`
	Type       string `json:"instance_type"`
	AZ         string `json:"az"`
	PrivateIP  string `json:"private_ip"`
	// SSM is the agent's ping status: Online, ConnectionLost, Inactive or
	// empty if the instance never registered.
	SSM string `json:"ssm"`
	// Provision is the status of the node's setup-node.sh association
	// (mgx-<role>-<instance>): Success, Failed, Pending, or empty if it has not
	// reported yet. Apply returns before it finishes.
	Provision       string `json:"provision"`
	ProvisionDetail string `json:"provision_detail,omitempty"`
}

// Provisioned reports whether setup-node.sh finished successfully.
func (n Node) Provisioned() bool { return n.Provision == "Success" }

// ProvisionFailed reports whether setup-node.sh failed on the node.
func (n Node) ProvisionFailed() bool { return n.Provision == "Failed" }

// Nodes describes instances and their SSM agent status, in the given order.
func (c *Client) Nodes(ctx context.Context, ids []string) ([]Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	out, err := c.EC2.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: ids})
	if err != nil {
		return nil, err
	}
	byID := map[string]Node{}
	for _, r := range out.Reservations {
		for _, i := range r.Instances {
			n := Node{
				InstanceID: aws.ToString(i.InstanceId),
				Type:       string(i.InstanceType),
				PrivateIP:  aws.ToString(i.PrivateIpAddress),
			}
			if i.State != nil {
				n.State = string(i.State.Name)
			}
			if i.Placement != nil {
				n.AZ = aws.ToString(i.Placement.AvailabilityZone)
			}
			byID[n.InstanceID] = n
		}
	}
	info, err := c.SSM.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
		Filters: []ssmtypes.InstanceInformationStringFilter{{Key: aws.String("InstanceIds"), Values: ids}},
	})
	if err == nil {
		for _, ii := range info.InstanceInformationList {
			if n, ok := byID[aws.ToString(ii.InstanceId)]; ok {
				n.SSM = string(ii.PingStatus)
				byID[n.InstanceID] = n
			}
		}
	}
	res := make([]Node, 0, len(ids))
	for _, id := range ids {
		n, ok := byID[id]
		if !ok {
			continue
		}
		if n.Provision, n.ProvisionDetail, err = c.provisionStatus(ctx, id); err != nil {
			return nil, err
		}
		res = append(res, n)
	}
	return res, nil
}

// provisionStatus reads the status of the node's mgx-* association, the one
// that runs setup-node.sh.
func (c *Client) provisionStatus(ctx context.Context, id string) (status, detail string, err error) {
	p := ssm.NewDescribeInstanceAssociationsStatusPaginator(c.SSM, &ssm.DescribeInstanceAssociationsStatusInput{InstanceId: aws.String(id)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return "", "", fmt.Errorf("provisioning status of %s: %w", id, err)
		}
		for _, a := range page.InstanceAssociationStatusInfos {
			if !strings.HasPrefix(aws.ToString(a.AssociationName), "mgx-") {
				continue
			}
			status = aws.ToString(a.Status)
			if status != "Success" {
				detail = strings.TrimSpace(aws.ToString(a.DetailedStatus) + " " + aws.ToString(a.ErrorCode))
			}
			return status, detail, nil
		}
	}
	return "", "", nil
}

type ExecResult struct {
	InstanceID string `json:"instance_id"`
	Status     string `json:"status"`
	ExitCode   int32  `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
}

// Exec runs a shell command on instances through SSM Run Command and waits for
// every result. Output is truncated by SSM at 24 KB per stream.
func (c *Client) Exec(ctx context.Context, ids []string, command string, timeout time.Duration) ([]ExecResult, error) {
	sent, err := c.SSM.SendCommand(ctx, &ssm.SendCommandInput{
		DocumentName:   aws.String("AWS-RunShellScript"),
		InstanceIds:    ids,
		Parameters:     map[string][]string{"commands": {command}},
		TimeoutSeconds: aws.Int32(int32(timeout.Seconds())),
		Comment:        aws.String("bbctl exec"),
	})
	if err != nil {
		return nil, err
	}
	cmdID := sent.Command.CommandId
	waiter := ssm.NewCommandExecutedWaiter(c.SSM)
	var res []ExecResult
	for _, id := range ids {
		in := &ssm.GetCommandInvocationInput{CommandId: cmdID, InstanceId: aws.String(id)}
		// The waiter errors on any non-success status; the invocation below
		// still has the output, which is what the caller wants.
		_ = waiter.Wait(ctx, in, timeout+30*time.Second)
		inv, err := c.SSM.GetCommandInvocation(ctx, in)
		if err != nil {
			return res, fmt.Errorf("%s: %w", id, err)
		}
		res = append(res, ExecResult{
			InstanceID: id,
			Status:     string(inv.Status),
			ExitCode:   inv.ResponseCode,
			Stdout:     aws.ToString(inv.StandardOutputContent),
			Stderr:     aws.ToString(inv.StandardErrorContent),
		})
	}
	return res, nil
}

// ErrNoSessionPlugin means interactive sessions need the Session Manager plugin.
var ErrNoSessionPlugin = errors.New("session-manager-plugin not found on PATH")

// Session opens an interactive shell (localPort == 0) or forwards localPort
// to remotePort on the instance. It hands the SSM session to the Session
// Manager plugin, the same way the aws CLI does.
func (c *Client) Session(ctx context.Context, id string, remotePort, localPort int, profile string) error {
	plugin, err := exec.LookPath("session-manager-plugin")
	if err != nil {
		return ErrNoSessionPlugin
	}
	in := &ssm.StartSessionInput{Target: aws.String(id)}
	if localPort > 0 {
		in.DocumentName = aws.String("AWS-StartPortForwardingSession")
		in.Parameters = map[string][]string{
			"portNumber":      {fmt.Sprint(remotePort)},
			"localPortNumber": {fmt.Sprint(localPort)},
		}
	}
	out, err := c.SSM.StartSession(ctx, in)
	if err != nil {
		return err
	}
	resp, _ := json.Marshal(map[string]string{
		"SessionId":  aws.ToString(out.SessionId),
		"TokenValue": aws.ToString(out.TokenValue),
		"StreamUrl":  aws.ToString(out.StreamUrl),
	})
	params, _ := json.Marshal(in)
	endpoint := fmt.Sprintf("https://ssm.%s.amazonaws.com", c.Region())
	cmd := exec.CommandContext(ctx, plugin, string(resp), c.Region(), "StartSession", profile, string(params), endpoint)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
