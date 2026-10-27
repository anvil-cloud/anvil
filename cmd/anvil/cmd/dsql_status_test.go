package cmd

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func TestDBItemResults(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	items := []map[string]types.AttributeValue{
		{"roleName": s("orders_writer"), "sk": s("DEFINITION"), "version": s("v2")},
		{"roleName": s("orders_writer"), "sk": s("STATUS#DEFINITION"), "state": s("applied"), "version": s("v1"), "updatedAt": s(now)},
		{"roleName": s("table#shop.orders"), "sk": s("DEFINITION"), "version": s("t1")},
		{"roleName": s("table#shop.orders"), "sk": s("STATUS#DEFINITION"), "state": s("failed"), "message": s("column \"x\": ..."), "version": s("t1"), "updatedAt": s(now)},
		{"roleName": s("orders_writer"), "sk": s("arn:aws:iam::1:role/testing-seed-role"), "version": s("m1")},
		{"roleName": s(dbSettingsKey), "sk": s("DEFINITION"), "version": s("x1")},
		{"roleName": s("legacy"), "sk": s("DEFINITION")}, // no version: older provider, not waited on
		{"roleName": s("evil"), "sk": s("DEFINITION")},   // written by hand, rejected
		{"roleName": s("evil"), "sk": s("STATUS#DEFINITION"), "state": s("failed"), "message": s("rejected"), "updatedAt": s(now)},
	}
	results := dbItemResults(items)
	if len(results) != 5 {
		t.Fatalf("got %d results, want 5 (legacy skipped, the rejected unversioned item reported)", len(results))
	}
	byLabel := map[string]dbItemResult{}
	for _, r := range results {
		byLabel[r.label()] = r
	}
	if r := byLabel["role orders_writer"]; r.confirmed() {
		t.Error("status for an older version counted as confirmed")
	}
	if r := byLabel["table shop.orders"]; !r.confirmed() || r.Status.State != "failed" {
		t.Errorf("table result = %+v, want confirmed failed", r.Status)
	}
	if _, ok := byLabel["access testing-seed-role → orders_writer"]; !ok {
		t.Errorf("mapping label missing: %v", byLabel)
	}
	if r, ok := byLabel["roles"]; !ok || r.Status != nil {
		t.Errorf("settings result = %+v, want present with no status", r)
	}
	if r, ok := byLabel["role evil"]; !ok || !r.confirmed() || r.Status.State != "failed" {
		t.Errorf("rejected unversioned item = %+v, want reported as failed", r)
	}
	if failed := printDatabaseChanges("shop", results, time.Now().Add(-time.Minute)); failed != 2 {
		t.Errorf("failed = %d, want 2", failed)
	}
}
