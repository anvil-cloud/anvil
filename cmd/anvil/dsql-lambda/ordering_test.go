package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/jackc/pgx/v5/pgconn"
)

func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	old := sleep
	sleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	t.Cleanup(func() { sleep = old })
	return &waits
}

var conflict = &pgconn.PgError{Code: "40001", Message: "schema has been updated by another transaction (OC001)"}

func TestWithRetry(t *testing.T) {
	ctx := context.Background()

	t.Run("retries conflicts until success", func(t *testing.T) {
		waits := noSleep(t)
		calls := 0
		err := withRetry(ctx, "stmt", func() error {
			calls++
			if calls < 3 {
				return conflict
			}
			return nil
		})
		if err != nil || calls != 3 {
			t.Fatalf("err=%v calls=%d, want nil after 3 calls", err, calls)
		}
		if len(*waits) != 2 {
			t.Fatalf("waited %d times, want 2", len(*waits))
		}
		for i, w := range *waits {
			lo := (retryBase << i) / 2
			hi := retryBase << i
			if w < lo || w > hi {
				t.Errorf("wait %d = %s, want within [%s, %s]", i, w, lo, hi)
			}
		}
	})

	t.Run("returns other errors immediately", func(t *testing.T) {
		noSleep(t)
		other := &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}
		calls := 0
		err := withRetry(ctx, "stmt", func() error { calls++; return other })
		if !errors.Is(err, other) || calls != 1 {
			t.Fatalf("err=%v calls=%d, want the error after 1 call", err, calls)
		}
	})

	t.Run("gives up after the attempt limit", func(t *testing.T) {
		waits := noSleep(t)
		calls := 0
		err := withRetry(ctx, "stmt", func() error { calls++; return conflict })
		if !isConflict(err) || calls != retryAttempts {
			t.Fatalf("err=%v calls=%d, want conflict after %d calls", err, calls, retryAttempts)
		}
		for _, w := range *waits {
			if w > retryMax {
				t.Errorf("wait %s exceeds retryMax %s", w, retryMax)
			}
		}
	})

	t.Run("detects wrapped conflicts", func(t *testing.T) {
		if !isConflict(errors.Join(errors.New("ctx"), conflict)) {
			t.Error("wrapped 40001 not detected")
		}
		if isConflict(errors.New("40001")) {
			t.Error("plain error text treated as a conflict")
		}
	})
}

func TestBuildScopedGrantStatements(t *testing.T) {
	role := roleConfig{RoleName: "orders_read", Schema: "shop", Grants: []string{"SELECT", "INSERT"}}

	got := buildScopedGrantStatements(role, nil)
	want := []string{`GRANT USAGE ON SCHEMA "shop" TO "orders_read"`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("no tables: got %q, want USAGE only (never schema-wide)", got)
	}

	got = buildScopedGrantStatements(role, []string{"orders", "order_audit"})
	want = []string{
		`GRANT USAGE ON SCHEMA "shop" TO "orders_read"`,
		`GRANT SELECT, INSERT ON "shop"."orders" TO "orders_read"`,
		`GRANT SELECT, INSERT ON "shop"."order_audit" TO "orders_read"`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}

	// The full role spec is the scoped statements over every scoped table.
	role.TableScoping = []string{"orders", "order_audit"}
	if got := buildRoleGrantStatements(role); !reflect.DeepEqual(got, want) {
		t.Errorf("buildRoleGrantStatements scoped: got %q\nwant %q", got, want)
	}
}

type fakeScanner struct {
	pages []*dynamodb.ScanOutput
	input *dynamodb.ScanInput
	calls int
}

func (f *fakeScanner) Scan(_ context.Context, in *dynamodb.ScanInput, _ ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error) {
	f.input = in
	page := f.pages[f.calls]
	f.calls++
	return page, nil
}

func roleItem(name, schema string, grants, scoping []string) map[string]types.AttributeValue {
	list := func(vs []string) *types.AttributeValueMemberL {
		l := &types.AttributeValueMemberL{}
		for _, v := range vs {
			l.Value = append(l.Value, &types.AttributeValueMemberS{Value: v})
		}
		return l
	}
	return map[string]types.AttributeValue{
		"roleName":     &types.AttributeValueMemberS{Value: name},
		"sk":           &types.AttributeValueMemberS{Value: "DEFINITION"},
		"schema":       &types.AttributeValueMemberS{Value: schema},
		"grants":       list(grants),
		"tableScoping": list(scoping),
	}
}

func TestRolesScopedTo(t *testing.T) {
	setEnv(t)
	scanner := &fakeScanner{pages: []*dynamodb.ScanOutput{
		{
			Items:            []map[string]types.AttributeValue{roleItem("orders_read", "shop", []string{"SELECT"}, []string{"orders"})},
			LastEvaluatedKey: map[string]types.AttributeValue{"roleName": &types.AttributeValueMemberS{Value: "orders_read"}},
		},
		{
			Items: []map[string]types.AttributeValue{roleItem("orders_writer", "shop", []string{"SELECT", "INSERT"}, []string{"orders", "order_audit"})},
		},
	}}

	roles, err := rolesScopedTo(context.Background(), scanner, "shop", "orders")
	if err != nil {
		t.Fatal(err)
	}
	if scanner.calls != 2 {
		t.Errorf("scanned %d pages, want 2", scanner.calls)
	}
	want := []roleConfig{
		{RoleName: "orders_read", Schema: "shop", Grants: []string{"SELECT"}, TableScoping: []string{"orders"}},
		{RoleName: "orders_writer", Schema: "shop", Grants: []string{"SELECT", "INSERT"}, TableScoping: []string{"orders", "order_audit"}},
	}
	if !reflect.DeepEqual(roles, want) {
		t.Errorf("got %+v\nwant %+v", roles, want)
	}

	in := scanner.input
	if *in.TableName != testTable || !*in.ConsistentRead {
		t.Errorf("scan table=%q consistent=%v, want %q and a consistent read", *in.TableName, *in.ConsistentRead, testTable)
	}
	for k, v := range map[string]string{":def": "DEFINITION", ":schema": "shop", ":table": "orders"} {
		if got := in.ExpressionAttributeValues[k].(*types.AttributeValueMemberS).Value; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestMappingArns(t *testing.T) {
	setEnv(t)
	scanner := &fakeScanner{pages: []*dynamodb.ScanOutput{{Items: []map[string]types.AttributeValue{
		{"roleName": &types.AttributeValueMemberS{Value: "orders_writer"}, "sk": &types.AttributeValueMemberS{Value: "arn:aws:iam::1:role/a"}},
		{"roleName": &types.AttributeValueMemberS{Value: "orders_writer"}, "sk": &types.AttributeValueMemberS{Value: "arn:aws:iam::1:role/b"}},
	}}}}
	arns, err := mappingArns(context.Background(), scanner, "orders_writer")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(arns, []string{"arn:aws:iam::1:role/a", "arn:aws:iam::1:role/b"}) {
		t.Errorf("arns = %v", arns)
	}
	in := scanner.input
	if *in.FilterExpression != "roleName = :role AND begins_with(sk, :arn)" ||
		in.ExpressionAttributeValues[":role"].(*types.AttributeValueMemberS).Value != "orders_writer" {
		t.Errorf("filter = %q %v", *in.FilterExpression, in.ExpressionAttributeValues)
	}
}
