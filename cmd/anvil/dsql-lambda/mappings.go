package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/jackc/pgx/v5"
)

// ── Re-applying a role's IAM mappings ───────────────────────────────────────
//
// Dropping a role drops its IAM mappings with it. When the role is created
// again while its mapping items are unchanged in the table (e.g. a role item
// deleted by hand, then restored by `anvil deploy --refresh`), no mapping
// record fires, so the role would come back with nothing able to connect as
// it. Whenever a role is created, its mappings are re-applied from the table.

// mappingArns lists the IAM role ARNs the table maps to roleName.
func mappingArns(ctx context.Context, ddb tableScanner, roleName string) ([]string, error) {
	var arns []string
	pages := dynamodb.NewScanPaginator(ddb, &dynamodb.ScanInput{
		TableName:        aws.String(rolesTableName),
		ConsistentRead:   aws.Bool(true),
		FilterExpression: aws.String("roleName = :role AND begins_with(sk, :arn)"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":role": &types.AttributeValueMemberS{Value: roleName},
			":arn":  &types.AttributeValueMemberS{Value: "arn:"},
		},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scanning roles table for %q mappings: %w", roleName, err)
		}
		for _, item := range page.Items {
			if sk, ok := item["sk"].(*types.AttributeValueMemberS); ok {
				arns = append(arns, sk.Value)
			}
		}
	}
	return arns, nil
}

func reapplyMappings(ctx context.Context, conn *pgx.Conn, ddb tableScanner, roleName string) error {
	if ddb == nil {
		return nil
	}
	arns, err := mappingArns(ctx, ddb, roleName)
	if err != nil {
		return err
	}
	for _, arn := range arns {
		if !iamRoleArnPattern.MatchString(arn) {
			continue
		}
		log.Printf("[dsql-bootstrap]   re-applying IAM mapping %s → %q", arn, roleName)
		if err := execAll(ctx, conn, []string{
			fmt.Sprintf("AWS IAM GRANT %s TO %s", pgIdent(roleName), pgLiteral(arn)),
		}); err != nil {
			return err
		}
	}
	return nil
}
