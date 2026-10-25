package dsqlgrant

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/dynamodb"
	"github.com/pulumi/pulumi-aws/sdk/v7/go/aws/iam"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi-go-provider/infer"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// ── Overview ──────────────────────────────────────────────────────────────
//
// DSQLConnect wires a compute resource (Lambda, ECS, etc.) to an Anvil DSQL
// cluster. It is the single source of truth for DSQL connect permissions —
// both the Go provider (dsql.GrantConnect) and the TypeScript/Python SDKs
// (dsql.grantConnect) create this component under the hood.
//
// What it creates:
//
//   1. IAM policy (dsql:DbConnect) scoped to the cluster ARNs.
//      This allows the compute resource to generate IAM auth tokens
//      via getDbConnectAuthToken() / GenerateDbConnectAuthToken().
//
//   2. DynamoDB IAM mapping item (optional, when dbRole + rolesTableName
//      are provided). The item is written to the DSQL component's roles
//      table with sk = the target's IAM role ARN. The DynamoDB stream
//      fires an INSERT, and the bootstrap Lambda runs:
//        AWS IAM GRANT "dbRole" TO 'arn:aws:iam::...:role/target-role';
//      This maps the IAM identity to a Postgres database role so DSQL
//      knows which role to assign on connection.
//
// Cleanup is automatic: removing the DSQLConnect (or the grantConnect call)
// deletes the IAM policy and the DynamoDB item. The stream fires REMOVE,
// and the bootstrap Lambda runs AWS IAM REVOKE.
//
// VPC network access is NOT handled here. The DSQL component gives each
// interface endpoint SG self-referencing ingress and egress on 5432, so a
// compute resource that declares the endpoint SG in vpc.vpcEndpoints can
// reach the endpoint whichever language called grantConnect.
//
// Usage from user code (TypeScript):
//
//   dsql.grantConnect(lambda, 'app_role');
//
// Usage standalone (advanced — for compute types without grantConnect):
//
//   new anvil.aws.DSQLConnect('my-grant', {
//     clusterArns: dsql.clusterArns,
//     targetRoleArn: ecsTask.roleArn,
//     targetName: 'my-ecs-task',
//     dbRole: 'app_role',
//     rolesTableName: dsql.rolesTableName,
//   });
//
// Connection flow (what the compute resource does at runtime):
//
//   1. Generate token: signer.getDbConnectAuthToken() (NOT admin token)
//   2. Connect: user="app_role", password=token, port=5432, ssl=true
//   3. DSQL checks: does this IAM ARN have dsql:DbConnect? (IAM policy)
//   4. DSQL checks: is this IAM ARN mapped to "app_role"? (AWS IAM GRANT)
//   5. Connection established with "app_role" privileges
//
// ──────────────────────────────────────────────────────────────────────────

// ── Args ───────────────────────────────────────────────────────────────────

type DSQLConnectArgs struct {
	// ClusterArns is the map of region → cluster ARN from the DSQL component.
	// Used to scope the dsql:DbConnect IAM policy. Single-region has one entry,
	// multi-region has one per region.
	ClusterArns pulumi.StringMapInput `pulumi:"clusterArns"`

	// TargetRoleArn is the IAM execution role ARN of the compute resource
	// (Lambda, ECS task, etc.) that will connect to the cluster.
	// The IAM policy is attached to this role, and the IAM mapping item
	// uses this ARN as the DynamoDB sort key.
	TargetRoleArn pulumi.StringInput `pulumi:"targetRoleArn"`

	// TargetName is the logical name of the compute resource.
	// Used for naming child resources (e.g. "api-handler").
	TargetName string `pulumi:"targetName"`

	// DbRoles are the Postgres database role names to map this IAM identity to.
	// One mapping item is created per role; the bootstrap Lambda runs:
	//   AWS IAM GRANT "<role>" TO 'arn:aws:iam::...:role/target-role';
	// for each. The compute connects as whichever role it chooses at runtime.
	// When empty, only the IAM policy is created — no database-level mapping.
	DbRoles []string `pulumi:"dbRoles,optional"`

	// RolesTableName is the DynamoDB table name for role bootstrap.
	// Passed from dsql.rolesTableName. When set (along with dbRole),
	// a TableItem is created that triggers the bootstrap Lambda to run
	// AWS IAM GRANT. When the DSQL component has no roles configured,
	// this is empty and the mapping item is skipped.
	RolesTableName pulumi.StringInput `pulumi:"rolesTableName,optional"`
}

// ── Component ──────────────────────────────────────────────────────────────

type DSQLConnect struct {
	pulumi.ResourceState

	name string
}

func (g *DSQLConnect) Annotate(a infer.Annotator) {
	a.SetToken("aws", "DSQLConnect")
	a.Describe(&g, "Grants a compute resource (Lambda, ECS, etc.) connect access to an Anvil DSQL cluster. Creates the IAM policy (dsql:DbConnect) and optionally a DynamoDB IAM mapping item that triggers the bootstrap Lambda to run AWS IAM GRANT, mapping the compute resource's IAM role to a Postgres database role.")
}

func NewDSQLConnect(ctx *pulumi.Context, name string, args DSQLConnectArgs, opts ...pulumi.ResourceOption) (*DSQLConnect, error) {
	g := &DSQLConnect{name: name}

	if err := ctx.RegisterComponentResource(p.GetTypeToken(ctx), name, g, opts...); err != nil {
		return nil, err
	}

	// Build mode runs a metadata-only preview (anvil generate / deploy schema
	// discovery). DSQLConnect contributes no schema or function metadata, and
	// its inputs (e.g. the target role ARN) are empty in build mode — attempting
	// to create the real IAM/DynamoDB resources would error and tear down the
	// discovery preview before the manifest is fully written. Short-circuit to a
	// registered no-op.
	if os.Getenv("ANVIL_BUILD_MODE") == "true" {
		ctx.RegisterResourceOutputs(g, pulumi.Map{})
		return g, nil
	}

	// ── 1. IAM policy — dsql:DbConnect on all cluster ARNs ────────────
	// Allows the compute resource to call getDbConnectAuthToken() /
	// GenerateDbConnectAuthToken() to generate temporary auth tokens.
	// Scoped to all cluster ARNs (one per region for multi-region).
	policyJSON := args.ClusterArns.ToStringMapOutput().ApplyT(func(arns map[string]string) (string, error) {
		resources := make([]string, 0, len(arns))
		for _, arn := range arns {
			resources = append(resources, arn)
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
				Action:   []string{"dsql:DbConnect"},
				Resource: resources,
			}},
		})
		if err != nil {
			return "", fmt.Errorf("failed to marshal dsql:DbConnect policy: %w", err)
		}
		return string(b), nil
	}).(pulumi.StringOutput)

	roleNameOutput := args.TargetRoleArn.ToStringOutput().ApplyT(func(arn string) string {
		for i := len(arn) - 1; i >= 0; i-- {
			if arn[i] == '/' {
				return arn[i+1:]
			}
		}
		return arn
	}).(pulumi.StringOutput)

	if _, err := iam.NewRolePolicy(ctx,
		fmt.Sprintf("%s-policy", name),
		&iam.RolePolicyArgs{
			Role:   roleNameOutput,
			Policy: policyJSON,
		},
		pulumi.Parent(g),
	); err != nil {
		return nil, fmt.Errorf("creating dsql:DbConnect policy: %w", err)
	}

	// ── 2. IAM mapping items — trigger bootstrap Lambda ───────────────
	// One item per database role. PK = role name, SK = target's IAM role ARN.
	// The DynamoDB stream fires INSERT → bootstrap Lambda runs:
	//   AWS IAM GRANT "<role>" TO 'arn:aws:iam::...:role/target-role';
	// On delete, stream fires REMOVE → bootstrap Lambda runs AWS IAM REVOKE.
	// Skipped when no roles or no rolesTableName (roles not configured).
	if args.RolesTableName != nil {
		for _, dbRole := range args.DbRoles {
			dbRole := dbRole
			itemJSON := args.TargetRoleArn.ToStringOutput().ApplyT(func(arn string) (string, error) {
				item := map[string]map[string]string{
					"roleName": {"S": dbRole},
					"sk":       {"S": arn},
				}
				b, err := json.Marshal(item)
				if err != nil {
					return "", fmt.Errorf("failed to marshal IAM mapping item: %w", err)
				}
				return string(b), nil
			}).(pulumi.StringOutput)

			if _, err := dynamodb.NewTableItem(ctx,
				fmt.Sprintf("%s-iam-mapping-%s", name, dbRole),
				&dynamodb.TableItemArgs{
					TableName: args.RolesTableName.ToStringOutput(),
					HashKey:   pulumi.String("roleName"),
					RangeKey:  pulumi.String("sk"),
					Item:      itemJSON,
				},
				pulumi.Parent(g),
			); err != nil {
				return nil, fmt.Errorf("creating IAM mapping for %s → %s: %w", args.TargetName, dbRole, err)
			}
		}
	}

	ctx.RegisterResourceOutputs(g, pulumi.Map{})

	return g, nil
}
