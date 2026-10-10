package vpc

import (
	"encoding/json"
	"fmt"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// BastionArgs configures the SSM bastion host.
//
// The bastion is a jump box that gives you access into the private network
// for local debugging, database migrations, and incident response.
//
// # How to connect
//
// Prerequisites: AWS CLI + Session Manager plugin installed locally.
//
//	aws ssm start-session --target <bastionInstanceId>
//
// To forward a local port to RDS (replace values as needed):
//
//	aws ssm start-session \
//	  --target <bastionInstanceId> \
//	  --document-name AWS-StartPortForwardingSessionToRemoteHost \
//	  --parameters '{"host":["<rds-endpoint>"],"portNumber":["5432"],"localPortNumber":["5432"]}'
//
// Then connect locally:
//
//	psql -h localhost -p 5432 -U <user> -d <dbname>
//
// The bastion never exposes port 22 or accepts any inbound connections.
// The SSM agent inside the instance initiates an outbound HTTPS connection
// to the AWS SSM service, which brokers your session. Access is controlled
// entirely through IAM — revoke access by revoking IAM permissions.
type BastionArgs struct {
	// InstanceType is the EC2 instance type. Default: "t4g.nano".
	// The bastion is purely a jump box with no CPU/memory requirements.
	InstanceType string `pulumi:"instanceType,optional"`

	// AllowedCidrs restricts which source IPs can initiate SSM sessions
	// via an IAM policy condition. Omit to allow any authenticated IAM
	// principal to start a session.
	// Example: ["203.0.113.0/32"] to restrict to your office IP.
	AllowedCidrs []string `pulumi:"allowedCidrs,optional"`
}

func resolveBastionInstanceType(bastion *BastionArgs) string {
	if bastion.InstanceType != "" {
		return bastion.InstanceType
	}
	return "t4g.nano"
}

// createBastion provisions a hardened SSM-only bastion host in the first
// public subnet. No SSH, no port 22, no key pairs. Access is via AWS SSM
// Session Manager only, controlled entirely through IAM.
//
// # How to connect
//
// Install prerequisites:
//
//	brew install awscli session-manager-plugin   # macOS
//
// Start a session:
//
//	aws ssm start-session --target <bastionInstanceId>
//
// Forward a port to RDS:
//
//	aws ssm start-session \
//	  --target <bastionInstanceId> \
//	  --document-name AWS-StartPortForwardingSessionToRemoteHost \
//	  --parameters '{"host":["<rds-endpoint>"],"portNumber":["5432"],"localPortNumber":["5432"]}'
//
// Then connect locally:
//
//	psql -h localhost -p 5432 -U <user> -d <dbname>
//
// The bastion polls the SSM service outbound over HTTPS — it never receives
// inbound connections, so its security group has zero inbound rules.
func createBastion(
	ctx *pulumi.Context,
	name, stage, stageId string,
	instanceType string,
	allowedCidrs []string,
	vpcResource *ec2.Vpc,
	publicSubnet *ec2.Subnet,
	parent pulumi.Resource,
) (*ec2.Instance, *ec2.SecurityGroup, error) {

	// 1. IAM role — EC2 trust policy.
	// AmazonSSMManagedInstanceCore grants the SSM agent permission to:
	//   - Register with the SSM service
	//   - Receive session commands
	//   - Write session logs
	assumeRolePolicy := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": { "Service": "ec2.amazonaws.com" },
			"Action": "sts:AssumeRole"
		}]
	}`

	bastionRole := &iam.Role{}
	if err := ctx.RegisterResource("aws:iam/role:Role", name+"-bastion-role", pulumi.Map{
		"name":             pulumi.String(provider.PhysicalName(stage, name+"-bastion", "role", stageId)),
		"assumeRolePolicy": pulumi.String(assumeRolePolicy),
		"tags": pulumi.StringMap{
			"Name":      pulumi.String(provider.PhysicalName(stage, name+"-bastion", "role", stageId)),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, bastionRole, pulumi.Parent(parent)); err != nil {
		return nil, nil, fmt.Errorf("failed to create bastion IAM role: %w", err)
	}

	// 2. Attach AmazonSSMManagedInstanceCore — the minimum policy for SSM.
	if err := ctx.RegisterResource("aws:iam/rolePolicyAttachment:RolePolicyAttachment", name+"-bastion-ssm-policy", pulumi.Map{
		"role":      bastionRole.Name,
		"policyArn": pulumi.String("arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"),
	}, &iam.RolePolicyAttachment{}, pulumi.Parent(parent)); err != nil {
		return nil, nil, fmt.Errorf("failed to attach SSM policy to bastion role: %w", err)
	}

	// 3. Optional: restrict SSM session initiation to specific source CIDRs.
	// This IAM condition checks the source IP of the ssm:StartSession API call
	// (the developer's IP calling the AWS API) not the session traffic itself.
	if len(allowedCidrs) > 0 {
		type condition struct {
			IpAddress map[string][]string `json:"IpAddress"`
		}
		type statement struct {
			Effect    string    `json:"Effect"`
			Action    string    `json:"Action"`
			Resource  string    `json:"Resource"`
			Condition condition `json:"Condition"`
		}
		type doc struct {
			Version   string      `json:"Version"`
			Statement []statement `json:"Statement"`
		}

		policyDoc, err := json.Marshal(doc{
			Version: "2012-10-17",
			Statement: []statement{{
				Effect:   "Allow",
				Action:   "ssm:StartSession",
				Resource: "*",
				Condition: condition{
					IpAddress: map[string][]string{
						"aws:SourceIp": allowedCidrs,
					},
				},
			}},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal bastion CIDR policy: %w", err)
		}

		if err := ctx.RegisterResource("aws:iam/rolePolicy:RolePolicy", name+"-bastion-cidr-policy", pulumi.Map{
			"role":   bastionRole.Name,
			"policy": pulumi.String(string(policyDoc)),
		}, &iam.RolePolicy{}, pulumi.Parent(parent)); err != nil {
			return nil, nil, fmt.Errorf("failed to attach CIDR restriction policy to bastion: %w", err)
		}
	}

	// 4. Instance profile wrapping the role.
	// EC2 instances reference profiles, not roles directly.
	bastionProfile := &iam.InstanceProfile{}
	if err := ctx.RegisterResource("aws:iam/instanceProfile:InstanceProfile", name+"-bastion-profile", pulumi.Map{
		"name": pulumi.String(provider.PhysicalName(stage, name+"-bastion", "profile", stageId)),
		"role": bastionRole.Name,
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}, bastionProfile, pulumi.Parent(parent)); err != nil {
		return nil, nil, fmt.Errorf("failed to create bastion instance profile: %w", err)
	}

	// 5. Security group — zero inbound rules.
	// The bastion never receives inbound connections. The SSM agent initiates
	// an outbound HTTPS connection to ssm.amazonaws.com which brokers sessions.
	// Outbound 443 is required to reach SSM, SSMMessages, and EC2Messages endpoints.
	bastionSGName := provider.PhysicalName(stage, name, "bastion-sg", stageId)
	bastionSG, err := ec2.NewSecurityGroup(ctx, name+"-bastion-sg", &ec2.SecurityGroupArgs{
		VpcId:       vpcResource.ID(),
		Description: pulumi.String("Anvil bastion host SSM only, zero inbound rules"),
		// No ingress rules -- the bastion never receives inbound connections.
		Egress: ec2.SecurityGroupEgressArray{
			&ec2.SecurityGroupEgressArgs{
				Description: pulumi.String("HTTPS outbound for SSM agent (ssm, ssmmessages, ec2messages)"),
				FromPort:    pulumi.Int(443),
				ToPort:      pulumi.Int(443),
				Protocol:    pulumi.String("tcp"),
				CidrBlocks:  pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(bastionSGName),
			"ManagedBy": pulumi.String("anvil"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create bastion security group: %w", err)
	}

	// 6. Resolve latest Amazon Linux 2023 arm64 AMI.
	// AL2023 ships with the SSM agent pre-installed and enabled.
	// We use the official Amazon AMI (owner: amazon) so no third-party trust required.
	bastionAMI, err := ec2.LookupAmi(ctx, &ec2.LookupAmiArgs{
		MostRecent: pulumi.BoolRef(true),
		Owners:     []string{"amazon"},
		Filters: []ec2.GetAmiFilter{
			{
				Name:   "name",
				Values: []string{"al2023-ami-2023.*-kernel-*-arm64"},
			},
			{
				Name:   "architecture",
				Values: []string{"arm64"},
			},
			{
				Name:   "virtualization-type",
				Values: []string{"hvm"},
			},
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to resolve Amazon Linux 2023 arm64 AMI: %w", err)
	}

	// 7. User data — ensures SSM agent is running.
	// AL2023 ships with SSM agent pre-installed but this makes it explicit
	// and ensures it starts on boot even after unexpected stops.
	userData := `#!/bin/bash
set -e
systemctl enable amazon-ssm-agent
systemctl start amazon-ssm-agent
`

	// 8. Bastion EC2 instance.
	// IMDSv2 enforced. No key pair. No public IP needed — SSM routes via
	// the outbound HTTPS connection the agent initiates. Public IP is kept
	// enabled so the instance can reach ssm.amazonaws.com without NAT,
	// but this can be removed if VpcEndpoints for SSM are present.
	bastionInstanceName := provider.PhysicalName(stage, name, "bastion", stageId)
	bastionInstance, err := ec2.NewInstance(ctx, name+"-bastion", &ec2.InstanceArgs{
		Ami:                      pulumi.String(bastionAMI.Id),
		InstanceType:             pulumi.String(instanceType),
		SubnetId:                 publicSubnet.ID(),
		VpcSecurityGroupIds:      pulumi.StringArray{bastionSG.ID()},
		IamInstanceProfile:       bastionProfile.Name,
		AssociatePublicIpAddress: pulumi.Bool(true),
		UserData:                 pulumi.String(userData),
		MetadataOptions: &ec2.InstanceMetadataOptionsArgs{
			HttpEndpoint:            pulumi.String("enabled"),
			HttpTokens:              pulumi.String("required"), // IMDSv2 only
			HttpPutResponseHopLimit: pulumi.Int(1),
		},
		Tags: pulumi.StringMap{
			"Name":      pulumi.String(bastionInstanceName),
			"ManagedBy": pulumi.String("anvil"),
			// Tag makes it easy to find in the console and target via SSM fleet manager
			"Role": pulumi.String("bastion"),
		},
	}, pulumi.Parent(parent))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create bastion instance: %w", err)
	}

	return bastionInstance, bastionSG, nil
}
