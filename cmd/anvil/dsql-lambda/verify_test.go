package main

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	testTable  = "dev-db-dsql-roles-abc123"
	testStream = "arn:aws:dynamodb:ap-southeast-2:123456789012:table/dev-db-dsql-roles-abc123/stream/2026-10-27T00:00:00.000"
	testArn    = "arn:aws:iam::123456789012:role/dev-orders-api-role"
)

type fakeTable struct {
	item map[string]types.AttributeValue
	err  error
}

func (f fakeTable) GetItem(_ context.Context, _ *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	return &dynamodb.GetItemOutput{Item: f.item}, f.err
}

func setEnv(t *testing.T) {
	t.Helper()
	oldTable, oldStream := rolesTableName, rolesStreamArn
	rolesTableName, rolesStreamArn = testTable, testStream
	t.Cleanup(func() { rolesTableName, rolesStreamArn = oldTable, oldStream })
}

func mappingRecord(eventName, sk string) events.DynamoDBEventRecord {
	image := map[string]events.DynamoDBAttributeValue{
		"roleName": events.NewStringAttribute("orders_api"),
		"sk":       events.NewStringAttribute(sk),
	}
	r := events.DynamoDBEventRecord{
		EventID:        "1",
		EventName:      eventName,
		EventSource:    "aws:dynamodb",
		EventSourceArn: testStream,
		Change:         events.DynamoDBStreamRecord{Keys: image},
	}
	if eventName == "REMOVE" {
		r.Change.OldImage = image
	} else {
		r.Change.NewImage = image
	}
	return r
}

func storedMapping(sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"roleName": &types.AttributeValueMemberS{Value: "orders_api"},
		"sk":       &types.AttributeValueMemberS{Value: sk},
	}
}

func TestVerifyRecord(t *testing.T) {
	setEnv(t)
	ctx := context.Background()

	t.Run("insert for an item in the table is processed", func(t *testing.T) {
		r := mappingRecord("INSERT", testArn)
		ok, err := verifyRecord(ctx, fakeTable{item: storedMapping(testArn)}, &r)
		if err != nil || !ok {
			t.Fatalf("got ok=%v err=%v, want processed", ok, err)
		}
	})

	t.Run("forged insert for an item not in the table is skipped", func(t *testing.T) {
		r := mappingRecord("INSERT", testArn)
		if ok, err := verifyRecord(ctx, fakeTable{}, &r); err != nil || ok {
			t.Fatalf("got ok=%v err=%v, want skipped", ok, err)
		}
	})

	t.Run("NewImage is replaced by the table's current item", func(t *testing.T) {
		r := mappingRecord("MODIFY", testArn)
		r.Change.NewImage["grants"] = events.NewListAttribute([]events.DynamoDBAttributeValue{
			events.NewStringAttribute("DELETE"),
		})
		stored := storedMapping(testArn)
		stored["grants"] = &types.AttributeValueMemberL{Value: []types.AttributeValue{
			&types.AttributeValueMemberS{Value: "SELECT"},
		}}
		ok, err := verifyRecord(ctx, fakeTable{item: stored}, &r)
		if err != nil || !ok {
			t.Fatalf("got ok=%v err=%v, want processed", ok, err)
		}
		if got := r.Change.NewImage["grants"].List()[0].String(); got != "SELECT" {
			t.Fatalf("NewImage grants = %q, want the stored SELECT", got)
		}
	})

	t.Run("forged remove for an item still in the table is skipped", func(t *testing.T) {
		r := mappingRecord("REMOVE", testArn)
		if ok, err := verifyRecord(ctx, fakeTable{item: storedMapping(testArn)}, &r); err != nil || ok {
			t.Fatalf("got ok=%v err=%v, want skipped", ok, err)
		}
	})

	t.Run("remove for an item gone from the table is processed", func(t *testing.T) {
		r := mappingRecord("REMOVE", testArn)
		if ok, err := verifyRecord(ctx, fakeTable{}, &r); err != nil || !ok {
			t.Fatalf("got ok=%v err=%v, want processed", ok, err)
		}
	})

	t.Run("record from another stream is rejected", func(t *testing.T) {
		r := mappingRecord("INSERT", testArn)
		r.EventSourceArn = "arn:aws:dynamodb:ap-southeast-2:123456789012:table/other/stream/x"
		if ok, err := verifyRecord(ctx, fakeTable{item: storedMapping(testArn)}, &r); err != nil || ok {
			t.Fatalf("got ok=%v err=%v, want rejected", ok, err)
		}
	})

	t.Run("record with no source is rejected", func(t *testing.T) {
		r := mappingRecord("INSERT", testArn)
		r.EventSource, r.EventSourceArn = "", ""
		if ok, err := verifyRecord(ctx, fakeTable{item: storedMapping(testArn)}, &r); err != nil || ok {
			t.Fatalf("got ok=%v err=%v, want rejected", ok, err)
		}
	})

	t.Run("table read failure is returned for retry", func(t *testing.T) {
		r := mappingRecord("INSERT", testArn)
		if _, err := verifyRecord(ctx, fakeTable{err: errors.New("throttled")}, &r); err == nil {
			t.Fatal("want error")
		}
	})

	t.Run("missing env rejects everything", func(t *testing.T) {
		rolesStreamArn = ""
		t.Cleanup(func() { rolesStreamArn = testStream })
		r := mappingRecord("INSERT", testArn)
		if ok, err := verifyRecord(ctx, fakeTable{item: storedMapping(testArn)}, &r); err != nil || ok {
			t.Fatalf("got ok=%v err=%v, want rejected", ok, err)
		}
	})
}

func TestIAMRoleArnPattern(t *testing.T) {
	valid := []string{
		testArn,
		"arn:aws:iam::123456789012:role/service-role/my_role+=,.@-1",
		"arn:aws-us-gov:iam::123456789012:role/x",
		"arn:aws-cn:iam::123456789012:role/x",
	}
	invalid := []string{
		"arn:x'; DROP SCHEMA orders CASCADE; --",
		"arn:aws:iam::123456789012:role/x'; DROP SCHEMA orders CASCADE; --",
		"arn:aws:iam::123456789012:role/x\n",
		"arn:aws:iam::123456789012:user/x",
		"arn:aws:iam::12345:role/x",
		"arn:aws:iam::123456789012:role/",
		"",
	}
	for _, s := range valid {
		if !iamRoleArnPattern.MatchString(s) {
			t.Errorf("rejected valid ARN %q", s)
		}
	}
	for _, s := range invalid {
		if iamRoleArnPattern.MatchString(s) {
			t.Errorf("accepted invalid ARN %q", s)
		}
	}
}

func TestPgLiteral(t *testing.T) {
	if got, want := pgLiteral("a'b"), "'a''b'"; got != want {
		t.Errorf("pgLiteral = %s, want %s", got, want)
	}
}
