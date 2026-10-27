package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ── Record verification ─────────────────────────────────────────────────────
//
// Nothing is granted lambda:InvokeFunction on this function, but an account
// principal with broad Lambda permissions could still invoke it directly with
// a hand-written "stream event". So the event payload is never trusted on its
// own: the roles table is the source of truth.
//
//   INSERT / MODIFY → the item must exist (consistent read); its CURRENT
//                     contents replace NewImage. Gone → skip (a later REMOVE
//                     record handles it).
//   REMOVE          → the item must be absent. Still present → skip, so a
//                     forged REMOVE can't revoke or drop anything.
//
// A direct invoke can therefore only replay what is already in the table —
// the same thing the stream would do. Writing to the table is the real
// boundary, and only Pulumi does that.
//
// OldImage (used by MODIFY to revoke the previous grants) isn't checked: the
// table no longer holds it. Its revokes are best-effort and the new grants are
// re-applied from the verified NewImage straight after.

var (
	rolesTableName = os.Getenv("ROLES_TABLE_NAME")
	rolesStreamArn = os.Getenv("ROLES_STREAM_ARN")
)

type itemGetter interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
}

// verifyRecord reports whether record should be processed, and for
// INSERT/MODIFY swaps its NewImage for the item as it is now in the table.
// An error means the table couldn't be read; the stream retries it.
func verifyRecord(ctx context.Context, ddb itemGetter, record *events.DynamoDBEventRecord) (bool, error) {
	if rolesTableName == "" || rolesStreamArn == "" {
		log.Printf("[dsql-bootstrap] REJECTED record %s: ROLES_TABLE_NAME/ROLES_STREAM_ARN not set", record.EventID)
		return false, nil
	}
	if record.EventSource != "aws:dynamodb" || record.EventSourceArn != rolesStreamArn {
		log.Printf("[dsql-bootstrap] REJECTED record %s: source %q %q is not the roles table stream",
			record.EventID, record.EventSource, record.EventSourceArn)
		return false, nil
	}

	roleName, hasRole := record.Change.Keys["roleName"]
	sk, hasSk := record.Change.Keys["sk"]
	if !hasRole || !hasSk || roleName.DataType() != events.DataTypeString || sk.DataType() != events.DataTypeString {
		log.Printf("[dsql-bootstrap] REJECTED record %s: missing roleName/sk keys", record.EventID)
		return false, nil
	}

	out, err := ddb.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(rolesTableName),
		ConsistentRead: aws.Bool(true),
		Key: map[string]types.AttributeValue{
			"roleName": &types.AttributeValueMemberS{Value: roleName.String()},
			"sk":       &types.AttributeValueMemberS{Value: sk.String()},
		},
	})
	if err != nil {
		return false, fmt.Errorf("reading %s/%s from roles table: %w", roleName.String(), sk.String(), err)
	}
	exists := len(out.Item) > 0

	switch record.EventName {
	case "INSERT", "MODIFY":
		if !exists {
			log.Printf("[dsql-bootstrap] SKIP record %s: %s for %s/%s but item is no longer in the table",
				record.EventID, record.EventName, roleName.String(), sk.String())
			return false, nil
		}
		image, err := fromSDKItem(out.Item)
		if err != nil {
			return false, fmt.Errorf("converting %s/%s: %w", roleName.String(), sk.String(), err)
		}
		record.Change.NewImage = image
		return true, nil

	case "REMOVE":
		if exists {
			log.Printf("[dsql-bootstrap] SKIP record %s: REMOVE for %s/%s but item is still in the table",
				record.EventID, roleName.String(), sk.String())
			return false, nil
		}
		return true, nil

	default:
		return false, nil
	}
}

// fromSDKItem converts a GetItem result into the stream event attribute type
// the rest of this Lambda reads.
func fromSDKItem(item map[string]types.AttributeValue) (map[string]events.DynamoDBAttributeValue, error) {
	out := make(map[string]events.DynamoDBAttributeValue, len(item))
	for k, v := range item {
		av, err := fromSDKValue(v)
		if err != nil {
			return nil, fmt.Errorf("attribute %q: %w", k, err)
		}
		out[k] = av
	}
	return out, nil
}

func fromSDKValue(v types.AttributeValue) (events.DynamoDBAttributeValue, error) {
	switch t := v.(type) {
	case *types.AttributeValueMemberS:
		return events.NewStringAttribute(t.Value), nil
	case *types.AttributeValueMemberN:
		return events.NewNumberAttribute(t.Value), nil
	case *types.AttributeValueMemberBOOL:
		return events.NewBooleanAttribute(t.Value), nil
	case *types.AttributeValueMemberNULL:
		return events.NewNullAttribute(), nil
	case *types.AttributeValueMemberB:
		return events.NewBinaryAttribute(t.Value), nil
	case *types.AttributeValueMemberSS:
		return events.NewStringSetAttribute(t.Value), nil
	case *types.AttributeValueMemberNS:
		return events.NewNumberSetAttribute(t.Value), nil
	case *types.AttributeValueMemberBS:
		return events.NewBinarySetAttribute(t.Value), nil
	case *types.AttributeValueMemberL:
		list := make([]events.DynamoDBAttributeValue, 0, len(t.Value))
		for _, e := range t.Value {
			av, err := fromSDKValue(e)
			if err != nil {
				return events.DynamoDBAttributeValue{}, err
			}
			list = append(list, av)
		}
		return events.NewListAttribute(list), nil
	case *types.AttributeValueMemberM:
		m, err := fromSDKItem(t.Value)
		if err != nil {
			return events.DynamoDBAttributeValue{}, err
		}
		return events.NewMapAttribute(m), nil
	default:
		return events.DynamoDBAttributeValue{}, fmt.Errorf("unsupported attribute type %T", v)
	}
}
