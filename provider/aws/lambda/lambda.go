package lambda

import (
	"encoding/json"
	"fmt"
	"os"

	provider "github.com/anvil-cloud/anvil/provider/internal/shared"
	"github.com/anvil-cloud/anvil/provider/internal/transform"
	"github.com/anvil-cloud/anvil/provider/internal/vpcsg"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/cloudwatch"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/lambda"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	c "github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// Compile-time check: Lambda must implement provider.GrantTarget
var _ provider.GrantTarget = (*Lambda)(nil)

// LambdaPermission defines an inline IAM permission for the Lambda execution role.
// Use this for ad-hoc permissions not covered by the grant system.
//
// Example:
//
//	permissions: [
//	    { actions: ["s3:GetObject", "s3:GetObjectVersion"], resource: bucket.arn },
//	    { actions: ["s3:GetObject"], resource: bucket.arn, path: "/uploads" },
//	]
type LambdaPermission struct {
	// Actions is the list of IAM actions to grant.
	Actions []string `pulumi:"actions"`

	// Resource is the ARN of the resource to grant access to.
	// Accepts plain strings and Pulumi outputs (e.g. bucket.arn).
	Resource interface{} `pulumi:"resource"`

	// Path is an optional path suffix appended to the resource ARN.
	// e.g. path: "/uploads" -> resource ARN + "/uploads/*"
	Path string `pulumi:"path,optional"`
}

// LambdaVpcEndpointArgs defines a VPC endpoint to grant this Lambda access to.
// Pass ep.securityGroupId and ep.endpointId from an Anvil VpcEndpoint.
// Both fields accept Output<string> -- pass the component outputs directly.
//
// Usage:
//
//	vpcEndpoints: [
//	    { securityGroupId: sns.securityGroupId, endpointId: sns.endpointId },
//	]
type LambdaVpcEndpointArgs struct {
	// SecurityGroupId is the endpoint's dedicated SG ID.
	// Use ep.securityGroupId from an Anvil VpcEndpoint.
	SecurityGroupId pulumi.StringInput `pulumi:"securityGroupId"`

	// EndpointId is the AWS VPC endpoint ID, e.g. vpce-0abc1234567890abc.
	// Use ep.endpointId from an Anvil VpcEndpoint.
	EndpointId pulumi.StringInput `pulumi:"endpointId"`
}

// LambdaVpcCidrArgs defines a CIDR-scoped egress rule.
// One SecurityGroupEgressRule is created per port per CIDR.
//
// Usage:
//
//	cidrs: [
//	    { range: "10.0.0.0/8", ports: [5432] },         // RDS in peered VPC
//	    { range: "172.16.0.0/12", ports: [6379, 6380] }, // ElastiCache cluster
//	]
type LambdaVpcCidrArgs struct {
	// Range is the IPv4 CIDR block to allow outbound traffic to.
	Range string `pulumi:"range"`

	// Ports is the list of TCP ports to allow to this CIDR.
	// Required -- be explicit about which ports each range needs.
	Ports []int `pulumi:"ports"`
}

// LambdaVpcArgs defines the VPC placement and network access configuration
// for a Lambda. All network access is declared here at construction time --
// nothing is reachable until explicitly declared.
type LambdaVpcArgs struct {
	// VpcId is the ID of the VPC to place the Lambda in.
	// Accepts Output<string> -- pass vpc.vpcId directly.
	VpcId pulumi.StringInput `pulumi:"vpcId"`

	// PrivateSubnetIds are the IDs of the private subnets to attach the Lambda to.
	// Always private -- Lambda must never be placed in public subnets.
	PrivateSubnetIds pulumi.StringArrayInput `pulumi:"privateSubnetIds"`

	// HasNat indicates whether this VPC has a NAT Gateway or fck-nat instance.
	// When true, Anvil adds an internet egress rule (port 443) automatically.
	// Only needed for imported VPCs -- omit when using an Anvil Vpc component.
	HasNat bool `pulumi:"hasNat,optional"`

	// VpcEndpoints are the VPC endpoints this Lambda needs access to.
	// Anvil wires the egress rule from this Lambda SG to each endpoint SG at
	// construction time. The endpoint SG's ingress is owned by the VpcEndpoint
	// constructor and is never modified here -- this prevents duplicate ingress
	// rule errors when multiple compute resources share the same endpoint.
	VpcEndpoints []LambdaVpcEndpointArgs `pulumi:"vpcEndpoints,optional"`

	// CIDRs defines structured CIDR-scoped egress rules.
	// Use for peered VPCs, on-premise ranges, or any non-AWS-service destination.
	CIDRs []LambdaVpcCidrArgs `pulumi:"cidrs,optional"`
}

// lambdaEndpointTarget adapts LambdaVpcEndpointArgs to satisfy vpcsg.EndpointTarget.
type lambdaEndpointTarget struct {
	args LambdaVpcEndpointArgs
}

func (t *lambdaEndpointTarget) Name() string {
	return "endpoint"
}

func (t *lambdaEndpointTarget) SecurityGroupOutput() pulumi.StringOutput {
	return t.args.SecurityGroupId.ToStringOutput()
}

// LambdaArgs defines the inputs for an Anvil-managed Lambda function.
type LambdaArgs struct {
	// Entry is the path to the function's entry point file, relative to
	// the anvil.config.ts directory. e.g. "backend/index.ts".
	// Omit to deploy a blank Lambda placeholder.
	Entry string `pulumi:"entry,optional"`

	// Runtime is the Lambda runtime identifier. e.g. "nodejs24.x"
	Runtime string `pulumi:"runtime,optional"`

	// Handler is the exported function name in the entry file.
	// e.g. "handler" for a function exported as `export const handler = ...`
	Handler string `pulumi:"handler,optional"`

	// Memory is the amount of memory in MB. Default: 1024.
	Memory int `pulumi:"memory,optional"`

	// Timeout is the function timeout in seconds. Default: 5.
	Timeout int `pulumi:"timeout,optional"`

	// Environment variables available to the function at runtime.
	// Values accept plain strings and Pulumi outputs (e.g. dsql.endpoint).
	Environment map[string]pulumi.StringInput `pulumi:"environment,optional"`

	// Architecture is the CPU architecture. Default: "arm64" (Graviton).
	Architecture string `pulumi:"architecture,optional"`

	// URL enables a direct HTTPS endpoint for the function.
	// Auth mode is AWS_IAM -- the endpoint is never public.
	URL bool `pulumi:"url,optional"`

	// Vpc places the Lambda inside a VPC for access to private resources.
	Vpc *LambdaVpcArgs `pulumi:"vpc,optional"`

	// Permissions defines inline IAM permissions added to the execution role.
	Permissions []LambdaPermission `pulumi:"permissions,optional"`

	// LogRetention sets the CloudWatch log group retention period.
	// Presets: "7d" | "30d" | "90d" | "1y" | "3y" | "6y" | "7y". Default: "1y"
	LogRetention string `pulumi:"logRetention,optional"`

	// Tracing enables AWS X-Ray tracing. Default: false.
	Tracing bool `pulumi:"tracing,optional"`

	// Encryption upgrades environment variable encryption to KMS.
	// Set to "kms" to enable.
	Encryption string `pulumi:"encryption,optional"`

	Transform map[string]map[string]interface{} `pulumi:"transform,optional"`
}

type Lambda struct {
	pulumi.ResourceState

	Arn             pulumi.StringOutput `pulumi:"arn"`
	FunctionName    pulumi.StringOutput `pulumi:"functionName"`
	RoleArn         pulumi.StringOutput `pulumi:"roleArn"`
	FunctionUrl     pulumi.StringOutput `pulumi:"functionUrl"`
	SecurityGroupID pulumi.StringOutput `pulumi:"securityGroupId"`
	name            string
}

func (l *Lambda) Name() string {
	return l.name
}

func (l *Lambda) RoleARN() pulumi.StringOutput {
	return l.RoleArn
}

func (l *Lambda) SecurityGroupId() pulumi.StringOutput {
	return l.SecurityGroupID
}

func (l *Lambda) Annotate(a infer.Annotator) {
	a.SetToken("aws", "Lambda")
	a.Describe(&l, "An Anvil-managed Lambda function. Tier 1 controls (least-privilege role, CloudWatch logs with 1y retention, arm64 architecture) are enforced automatically at no cost, satisfying the technical baseline of CIS Benchmarks, ISO 27017, SOC 2, NIST 800-53, and ISO 27001.")
}

func resolveDays(preset string) int {
	switch preset {
	case "7d":
		return 7
	case "30d":
		return 30
	case "90d":
		return 90
	case "1y":
		return 365
	case "3y":
		return 1095
	case "6y":
		return 2190
	case "7y":
		return 2555
	default:
		return 365
	}
}

func sanitizeCIDR(cidr string) string {
	result := make([]byte, len(cidr))
	for i := 0; i < len(cidr); i++ {
		c := cidr[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') {
			result[i] = c
		} else {
			result[i] = '-'
		}
	}
	return string(result)
}

const blankHandler = `exports.handler = async () => ({
  statusCode: 200,
  body: JSON.stringify({
    message: "This Lambda has no code deployed yet. Deploy your function code to replace this placeholder.",
    anvil: true,
  }),
});
`

func NewLambda(ctx *pulumi.Context, name string, args LambdaArgs, opts ...pulumi.ResourceOption) (*Lambda, error) {
	if os.Getenv("ANVIL_BUILD_MODE") == "true" {
		l := &Lambda{name: name}
		if args.Entry != "" {
			if err := appendToManifest(name, args); err != nil {
				return nil, fmt.Errorf("build manifest write failed for %s: %w", name, err)
			}
		}

		// Build mode runs a Pulumi preview to extract metadata without creating
		// real cloud resources. The component and its output properties must
		// still be registered: an unregistered component leaves the pulumi:"..."
		// output fields nil, which panics the provider when the engine
		// serializes the component's outputs.
		opts = provider.WithDefault(opts, true)
		if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, l, opts...); err != nil {
			return nil, err
		}

		emptyStr := pulumi.String("").ToStringOutput()
		l.Arn = emptyStr
		l.FunctionName = emptyStr
		l.RoleArn = emptyStr
		l.FunctionUrl = emptyStr
		l.SecurityGroupID = emptyStr

		ctx.RegisterResourceOutputs(l, pulumi.Map{
			"arn":             l.Arn,
			"functionName":    l.FunctionName,
			"roleArn":         l.RoleArn,
			"functionUrl":     l.FunctionUrl,
			"securityGroupId": l.SecurityGroupID,
		})

		return l, nil
	}

	l := &Lambda{name: name}

	provider.NewContext(ctx)

	cfg := c.New(ctx, "anvil")
	stage := cfg.Require("stage")
	stageId := cfg.Require("stageId")
	zipPath := cfg.Get(fmt.Sprintf("fn-%s", name))

	physicalName := provider.PhysicalName(stage, name, "lambda", stageId)

	memory := args.Memory
	if memory == 0 {
		memory = 1024
	}

	timeout := args.Timeout
	if timeout == 0 {
		timeout = 5
	}

	architecture := args.Architecture
	if architecture == "" {
		architecture = "arm64"
	}

	runtime := args.Runtime

	handler := args.Handler
	if handler == "" {
		handler = "handler"
	}

	logRetentionDays := resolveDays(args.LogRetention)

	tracingMode := "PassThrough"
	if args.Tracing {
		tracingMode = "Active"
	}

	opts = provider.WithDefault(opts, true)

	err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, l, opts...)
	if err != nil {
		return nil, err
	}

	// ── 1. CloudWatch Log Group ────────────────────────
	logGroupName := fmt.Sprintf("/aws/lambda/%s", physicalName)
	logGroupProps := transform.MergeTransform(args.Transform["logGroup"], pulumi.Map{
		"name":            pulumi.String(logGroupName),
		"retentionInDays": pulumi.Int(logRetentionDays),
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	})
	logGroup := &cloudwatch.LogGroup{}
	err = ctx.RegisterResource("aws:cloudwatch/logGroup:LogGroup", name+"-logs", logGroupProps, logGroup, pulumi.Parent(l))
	if err != nil {
		return nil, err
	}

	// ── 2. IAM Execution Role ──────────────────────────
	assumeRolePolicy := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Principal": { "Service": "lambda.amazonaws.com" },
			"Action": "sts:AssumeRole"
		}]
	}`

	role := &iam.Role{}
	roleProps := transform.MergeTransform(args.Transform["role"], pulumi.Map{
		"name":             pulumi.String(provider.PhysicalName(stage, name, "role", stageId)),
		"assumeRolePolicy": pulumi.String(assumeRolePolicy),
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	})
	err = ctx.RegisterResource("aws:iam/role:Role", name+"-role", roleProps, role, pulumi.Parent(l))
	if err != nil {
		return nil, err
	}

	// ── 3. Basic execution policy ──────────────────────
	// Captured as a variable so the Lambda function can DependsOn it,
	// ensuring IAM has fully propagated before the function is created.
	// This prevents InsufficientRolePermissions on VPC-attached Lambdas.
	var basicExecPolicyArn string
	if args.Vpc != nil {
		basicExecPolicyArn = "arn:aws:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole"
	} else {
		basicExecPolicyArn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
	}

	basicExec := &iam.RolePolicyAttachment{}
	err = ctx.RegisterResource("aws:iam/rolePolicyAttachment:RolePolicyAttachment", name+"-basic-exec", pulumi.Map{
		"role":      role.Name,
		"policyArn": pulumi.String(basicExecPolicyArn),
	}, basicExec, pulumi.Parent(l))
	if err != nil {
		return nil, err
	}

	// ── 4. Inline permissions ──────────────────────────
	for i, perm := range args.Permissions {
		i, perm := i, perm

		policyJSON := pulumi.ToOutput(perm.Resource).ApplyT(func(res interface{}) (string, error) {
			resource := fmt.Sprintf("%v", res)
			if perm.Path != "" {
				resource = fmt.Sprintf("%s/%s/*", resource, perm.Path)
			}

			type statement struct {
				Effect   string   `json:"Effect"`
				Action   []string `json:"Action"`
				Resource []string `json:"Resource"`
			}
			type doc struct {
				Version   string      `json:"Version"`
				Statement []statement `json:"Statement"`
			}

			b, err := json.Marshal(doc{
				Version: "2012-10-17",
				Statement: []statement{{
					Effect:   "Allow",
					Action:   perm.Actions,
					Resource: []string{resource},
				}},
			})
			if err != nil {
				return "", fmt.Errorf("failed to marshal permission %d: %w", i, err)
			}
			return string(b), nil
		}).(pulumi.StringOutput)

		err = ctx.RegisterResource("aws:iam/rolePolicy:RolePolicy",
			fmt.Sprintf("%s-perm-%d", name, i),
			pulumi.Map{
				"role":   role.Name,
				"policy": policyJSON,
			},
			&iam.RolePolicy{},
			pulumi.Parent(l),
		)
		if err != nil {
			return nil, err
		}
	}

	// ── 5. VPC security group + network wiring ─────────────────
	if args.Vpc != nil {
		sg, err := vpcsg.CreateSecurityGroup(ctx, name, stage, stageId, args.Vpc.VpcId, l)
		if err != nil {
			return nil, err
		}
		l.SecurityGroupID = sg.ID().ToStringOutput()

		targets := make([]vpcsg.EndpointTarget, len(args.Vpc.VpcEndpoints))
		for i, ep := range args.Vpc.VpcEndpoints {
			targets[i] = &lambdaEndpointTarget{args: ep}
		}
		if err := vpcsg.GrantEndpointAccess(
			ctx,
			name,
			l.SecurityGroupID,
			targets,
			l,
		); err != nil {
			return nil, fmt.Errorf("failed to wire endpoint egress: %w", err)
		}

		if args.Vpc.HasNat {
			if err := vpcsg.AddInternetEgressRule(
				ctx,
				fmt.Sprintf("%s-nat-egress", name),
				l.SecurityGroupID,
				443, 443,
				l,
			); err != nil {
				return nil, err
			}
		}

		for _, cidrRule := range args.Vpc.CIDRs {
			if len(cidrRule.Ports) == 0 {
				return nil, fmt.Errorf(
					"vpc.cidrs: range %q requires at least one port",
					cidrRule.Range,
				)
			}
			for _, port := range cidrRule.Ports {
				ruleName := fmt.Sprintf("%s-cidr-%s-%d", name, sanitizeCIDR(cidrRule.Range), port)
				if err := vpcsg.AddCIDREgressRule(
					ctx,
					ruleName,
					l.SecurityGroupID,
					cidrRule.Range,
					port, port,
					l,
				); err != nil {
					return nil, fmt.Errorf("vpc.cidrs: failed to wire %s:%d: %w", cidrRule.Range, port, err)
				}
			}
		}
	}

	// ── 6. Lambda Function ─────────────────────────────
	var codeArchive pulumi.Archive
	if zipPath != "" {
		codeArchive = pulumi.NewFileArchive(zipPath)
	} else {
		codeArchive = pulumi.NewAssetArchive(map[string]interface{}{
			"index.js": pulumi.NewStringAsset(blankHandler),
		})
	}

	lambdaMap := pulumi.Map{
		"name":          pulumi.String(physicalName),
		"runtime":       pulumi.String(runtime),
		"handler":       pulumi.String(handler),
		"role":          role.Arn,
		"memorySize":    pulumi.Int(memory),
		"timeout":       pulumi.Int(timeout),
		"architectures": pulumi.StringArray{pulumi.String(architecture)},
		"code":          codeArchive,
		"tracingConfig": pulumi.Map{
			"mode": pulumi.String(tracingMode),
		},
		"tags": pulumi.StringMap{
			"ManagedBy": pulumi.String("anvil"),
		},
	}

	if args.Vpc != nil {
		sgIds := pulumi.StringArray{l.SecurityGroupID}
		for _, ep := range args.Vpc.VpcEndpoints {
			sgIds = append(sgIds, ep.SecurityGroupId.ToStringOutput())
		}
		lambdaMap["vpcConfig"] = pulumi.Map{
			"subnetIds":        args.Vpc.PrivateSubnetIds.ToStringArrayOutput(),
			"securityGroupIds": sgIds,
		}
	}

	if len(args.Environment) > 0 {
		envVars := pulumi.StringMap{}
		for k, v := range args.Environment {
			envVars[k] = v.ToStringOutput()
		}
		lambdaMap["environment"] = pulumi.Map{
			"variables": envVars,
		}
	}

	if args.Encryption == "kms" {
		ctx.Log.Info("KMS encryption enabled for environment variables. Supply kmsKeyArn via transform[\"lambda\"].", nil)
	}

	lambdaProps := transform.MergeTransform(args.Transform["lambda"], lambdaMap)

	// DependsOn basicExec ensures IAM has fully propagated before Lambda is
	// created. Without this, AWS may reject the function with
	// InsufficientRolePermissions, especially for VPC-attached Lambdas which
	// need ENI management permissions from AWSLambdaVPCAccessExecutionRole.
	res := &lambda.Function{}
	err = ctx.RegisterResource("aws:lambda/function:Function", name, lambdaProps, res,
		pulumi.Parent(l),
		pulumi.DependsOn([]pulumi.Resource{logGroup, basicExec}),
	)
	if err != nil {
		return nil, err
	}

	// ── 7. Function URL ────────────────────────────────
	var functionUrl pulumi.StringOutput
	if args.URL {
		urlProps := transform.MergeTransform(args.Transform["url"], pulumi.Map{
			"functionName":      res.Name,
			"authorizationType": pulumi.String("AWS_IAM"),
		})
		urlRes := &lambda.FunctionUrl{}
		err = ctx.RegisterResource("aws:lambda/functionUrl:FunctionUrl", name+"-url", urlProps, urlRes, pulumi.Parent(l))
		if err != nil {
			return nil, err
		}
		functionUrl = urlRes.FunctionUrl
	}

	// ── 8. Wire outputs ────────────────────────────────
	l.Arn = res.Arn
	l.FunctionName = res.Name
	l.RoleArn = role.Arn
	if args.URL {
		l.FunctionUrl = functionUrl
	}

	ctx.RegisterResourceOutputs(l, pulumi.Map{
		"arn":             res.Arn,
		"functionName":    res.Name,
		"roleArn":         role.Arn,
		"functionUrl":     functionUrl,
		"securityGroupId": l.SecurityGroupID,
	})

	return l, nil
}

// ── Build manifest ─────────────────────────────────────────

type manifestEntry struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Runtime      string `json:"runtime"`
	Entry        string `json:"entry"`
	Handler      string `json:"handler"`
	Architecture string `json:"architecture"`
}

type manifest struct {
	Functions []manifestEntry `json:"functions"`
	// Schemas is preserved verbatim so a Lambda write does not clobber DSQL
	// schema entries already in the manifest (both components write to the
	// same file during a build-mode preview, in declaration order).
	Schemas []json.RawMessage `json:"schemas,omitempty"`
	// Compliance is preserved verbatim for the same reason (ComplianceScanner).
	Compliance json.RawMessage `json:"compliance,omitempty"`
}

func appendToManifest(name string, args LambdaArgs) error {
	const manifestPath = ".anvil/build-manifest.json"

	provider.ManifestMu.Lock()
	defer provider.ManifestMu.Unlock()

	if err := os.MkdirAll(".anvil", 0755); err != nil {
		return err
	}

	m := manifest{}
	if data, err := os.ReadFile(manifestPath); err == nil {
		_ = json.Unmarshal(data, &m)
	}

	architecture := args.Architecture
	if architecture == "" {
		architecture = "arm64"
	}

	m.Functions = append(m.Functions, manifestEntry{
		Name:         name,
		Type:         "anvil:aws:Lambda",
		Runtime:      args.Runtime,
		Entry:        args.Entry,
		Handler:      args.Handler,
		Architecture: architecture,
	})

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(manifestPath, data, 0644)
}
