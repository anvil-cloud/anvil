package main

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/jackc/pgx/v5/pgconn"
)

// ── Status items ────────────────────────────────────────────────────────────
//
// After acting on an item, the Lambda writes what happened next to it in the
// same table, so `anvil deploy` can wait for the result and report it:
//
//   roleName           sk                    state    message
//   orders_writer      STATUS#DEFINITION     applied  "SELECT, INSERT on shop.orders"
//   table#shop.orders  STATUS#DEFINITION     failed   "column \"channel\": ..."
//
// version is copied from the item it reports on (the provider writes a hash
// of the item's contents), so the CLI can tell this deploy's result from an
// older one. Status records on the stream are ignored by the handler, so
// writing them never triggers more work. A REMOVE deletes the item's status.

const (
	statusPrefix = "STATUS#"

	stateApplied = "applied"
	stateDrift   = "drift"
	stateFailed  = "failed"

	// settingsKey is the item holding component-level settings. Its status
	// carries the result of the role sweep.
	settingsKey = "anvil#settings"
)

// outcome is what processing an item did, for its status item. An empty
// State writes no status.
type outcome struct {
	State   string
	Message string
}

type statusWriter interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(ctx context.Context, in *dynamodb.DeleteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

// rolesTableAPI is everything the handler needs from the roles table.
type rolesTableAPI interface {
	itemGetter
	tableScanner
	statusWriter
}

func isStatusRecord(record events.DynamoDBEventRecord) bool {
	return strings.HasPrefix(record.Change.Keys["sk"].String(), statusPrefix)
}

// writeStatus records o against the item (pk, sk). A failed write is logged,
// never returned: the status is a report, not part of the change.
func writeStatus(ctx context.Context, ddb statusWriter, pk, sk, version string, o outcome) {
	if ddb == nil || o.State == "" {
		return
	}
	_, err := ddb.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(rolesTableName),
		Item: map[string]types.AttributeValue{
			"roleName":  &types.AttributeValueMemberS{Value: pk},
			"sk":        &types.AttributeValueMemberS{Value: statusPrefix + sk},
			"kind":      &types.AttributeValueMemberS{Value: "status"},
			"state":     &types.AttributeValueMemberS{Value: o.State},
			"message":   &types.AttributeValueMemberS{Value: o.Message},
			"version":   &types.AttributeValueMemberS{Value: version},
			"updatedAt": &types.AttributeValueMemberS{Value: time.Now().UTC().Format(time.RFC3339)},
		},
	})
	if err != nil {
		log.Printf("[dsql-bootstrap]   could not write status for %s/%s: %v", pk, sk, err)
		return
	}
	log.Printf("[dsql-bootstrap]   status %s/%s → %s: %s", pk, sk, o.State, o.Message)
}

func deleteStatus(ctx context.Context, ddb statusWriter, pk, sk string) {
	if ddb == nil {
		return
	}
	if _, err := ddb.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(rolesTableName),
		Key: map[string]types.AttributeValue{
			"roleName": &types.AttributeValueMemberS{Value: pk},
			"sk":       &types.AttributeValueMemberS{Value: statusPrefix + sk},
		},
	}); err != nil {
		log.Printf("[dsql-bootstrap]   could not delete status for %s/%s: %v", pk, sk, err)
	}
}

// permanentError marks a failure that retrying can't fix (bad item, SQL DSQL
// rejects). Its status is written and the record is not retried.
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// retryable reports whether the stream should retry a failed record:
// connection, resource and internal errors can pass; SQL DSQL rejected
// outright (unsupported, undefined object, dependency) can't.
func retryable(err error) bool {
	var perm permanentError
	if errors.As(err, &perm) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code[:2] {
		case "08", "40", "53", "57", "58", "XX":
			return true
		}
		return false
	}
	// Network, auth token and AWS API errors.
	return true
}
