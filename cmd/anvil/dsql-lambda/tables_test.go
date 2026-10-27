package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestExpectedType(t *testing.T) {
	// The right-hand side is what format_type() reports in DSQL.
	cases := map[string]columnDef{
		"uuid":                        {Type: "uuid"},
		"character varying(20)":       {Type: "varchar", Length: 20},
		"character varying":           {Type: "varchar"},
		"character(3)":                {Type: "char", Length: 3},
		"character(1)":                {Type: "char"},
		"numeric(10,2)":               {Type: "numeric", Precision: 10, Scale: 2},
		"numeric(12,0)":               {Type: "numeric", Precision: 12},
		"numeric(18,6)":               {Type: "numeric"},
		"time without time zone":      {Type: "time"},
		"timestamp without time zone": {Type: "timestamp"},
		"timestamp with time zone":    {Type: "timestamptz"},
		"double precision":            {Type: "double precision"},
		"jsonb":                       {Type: "jsonb"},
	}
	for want, c := range cases {
		if got := expectedType(c); got != want {
			t.Errorf("expectedType(%+v) = %q, want %q", c, got, want)
		}
	}
}

func ordersDef() tableDef {
	return tableDef{
		Schema: "shop", Name: "orders", PrimaryKey: []string{"id"},
		Columns: []columnDef{
			{Name: "id", Type: "uuid"},
			{Name: "status", Type: "text", NotNull: true, Default: "'pending'"},
			{Name: "notes", Type: "text"},
		},
		Indexes: []indexDef{{Name: "orders_status_idx", Columns: []string{"status"}}},
	}
}

func TestPlanTableChanges(t *testing.T) {
	existing := map[string]dbColumn{
		"id":     {Type: "uuid", NotNull: true},
		"status": {Type: "text", NotNull: true},
	}

	t.Run("adds a missing nullable column and a missing index", func(t *testing.T) {
		plan := planTableChanges(ordersDef(), existing, map[string]bool{"orders_pkey": true})
		if len(plan.AddColumns) != 1 || plan.AddColumns[0].Name != "notes" {
			t.Errorf("AddColumns = %+v, want notes", plan.AddColumns)
		}
		if len(plan.AddIndexes) != 1 || plan.AddIndexes[0].Name != "orders_status_idx" {
			t.Errorf("AddIndexes = %+v, want orders_status_idx", plan.AddIndexes)
		}
		if len(plan.Rejected)+len(plan.Drift) != 0 {
			t.Errorf("unexpected rejected=%v drift=%v", plan.Rejected, plan.Drift)
		}
	})

	t.Run("rejects NOT NULL or DEFAULT on a column added to an existing table", func(t *testing.T) {
		def := ordersDef()
		def.Columns = append(def.Columns,
			columnDef{Name: "channel", Type: "text", NotNull: true},
			columnDef{Name: "source", Type: "text", Default: "'web'", Unique: true},
		)
		plan := planTableChanges(def, existing, map[string]bool{"orders_status_idx": true})
		if len(plan.Rejected) != 2 {
			t.Fatalf("Rejected = %v, want channel and source", plan.Rejected)
		}
		for i, col := range []string{"channel", "source"} {
			if !strings.Contains(plan.Rejected[i], col) || !strings.Contains(plan.Rejected[i], "ALTER TABLE") {
				t.Errorf("Rejected[%d] = %q, want %s with the DSQL docs reference", i, plan.Rejected[i], col)
			}
		}
		for _, c := range plan.AddColumns {
			if c.Name == "channel" || c.Name == "source" {
				t.Errorf("rejected column %q was planned for adding", c.Name)
			}
		}
		for _, idx := range plan.AddIndexes {
			if idx.Name == "orders_source_key" {
				t.Error("unique index planned for a rejected column")
			}
		}
		if o := planOutcome(plan); o.State != stateFailed {
			t.Errorf("outcome state = %q, want failed", o.State)
		}
	})

	t.Run("reports a column whose type or nullability differs as drift", func(t *testing.T) {
		db := map[string]dbColumn{
			"id":     {Type: "uuid", NotNull: true},
			"status": {Type: "text", NotNull: false},
			"notes":  {Type: "integer"},
		}
		plan := planTableChanges(ordersDef(), db, map[string]bool{"orders_status_idx": true})
		if len(plan.AddColumns) != 0 {
			t.Errorf("AddColumns = %+v, want none (columns exist)", plan.AddColumns)
		}
		want := []string{
			`column "status" is NOT NULL in your code but allows NULL in the database`,
			`column "notes" is text in your code but integer in the database`,
		}
		if !reflect.DeepEqual(plan.Drift, want) {
			t.Errorf("Drift = %q\nwant %q", plan.Drift, want)
		}
		if o := planOutcome(plan); o.State != stateDrift {
			t.Errorf("outcome state = %q, want drift", o.State)
		}
	})

	t.Run("primary key columns are expected NOT NULL even when not declared so", func(t *testing.T) {
		plan := planTableChanges(ordersDef(), map[string]dbColumn{
			"id": {Type: "uuid", NotNull: true}, "status": {Type: "text", NotNull: true}, "notes": {Type: "text"},
		}, map[string]bool{"orders_status_idx": true})
		if len(plan.Drift) != 0 {
			t.Errorf("Drift = %q, want none", plan.Drift)
		}
		if o := planOutcome(plan); o != (outcome{State: stateApplied, Message: "up to date"}) {
			t.Errorf("outcome = %+v, want applied/up to date", o)
		}
	})

	t.Run("a column the code no longer has is left alone", func(t *testing.T) {
		db := map[string]dbColumn{
			"id": {Type: "uuid", NotNull: true}, "status": {Type: "text", NotNull: true},
			"notes": {Type: "text"}, "legacy": {Type: "text"},
		}
		plan := planTableChanges(ordersDef(), db, map[string]bool{"orders_status_idx": true})
		if len(plan.AddColumns)+len(plan.Drift)+len(plan.Rejected) != 0 {
			t.Errorf("plan = %+v, want nothing", plan)
		}
	})
}

func TestClassifyRoles(t *testing.T) {
	roles := []dbRole{
		{Name: "admin"},
		{Name: "dbowner", Super: true},
		{Name: "orders_writer", Tagged: true},
		{Name: "old_reader", Tagged: true},
		{Name: "hand_made"},
		{Name: "pg_monitor"},
	}
	declared := map[string]bool{"orders_writer": true}

	drop, unmanaged := classifyRoles(roles, declared, false)
	if !reflect.DeepEqual(drop, []string{"old_reader"}) || !reflect.DeepEqual(unmanaged, []string{"hand_made"}) {
		t.Errorf("prune off: drop=%v unmanaged=%v, want [old_reader] [hand_made]", drop, unmanaged)
	}

	drop, unmanaged = classifyRoles(roles, declared, true)
	if !reflect.DeepEqual(drop, []string{"hand_made", "old_reader"}) || len(unmanaged) != 0 {
		t.Errorf("prune on: drop=%v unmanaged=%v, want [hand_made old_reader] []", drop, unmanaged)
	}
}

func TestDescribeGrants(t *testing.T) {
	role := roleConfig{RoleName: "r", Schema: "shop", Grants: []string{"SELECT", "INSERT"}}
	if got := describeGrants(role, nil); got != "SELECT, INSERT on every table in shop" {
		t.Errorf("schema-wide: %q", got)
	}
	role.TableScoping = []string{"orders", "shipments"}
	if got := describeGrants(role, []string{"orders"}); got != "SELECT, INSERT on shop.orders (other scoped tables are granted when they're created)" {
		t.Errorf("partly granted: %q", got)
	}
	if got := describeGrants(role, []string{"orders", "shipments"}); got != "SELECT, INSERT on shop.orders, shop.shipments" {
		t.Errorf("fully granted: %q", got)
	}
}

func TestRetryable(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "0A000"}, false}, // feature not supported
		{&pgconn.PgError{Code: "42704"}, false}, // undefined object
		{&pgconn.PgError{Code: "2BP01"}, false}, // dependent objects
		{&pgconn.PgError{Code: "40001"}, true},  // concurrency conflict
		{&pgconn.PgError{Code: "08006"}, true},  // connection failure
		{errors.New("dial tcp: i/o timeout"), true},
		{permanent(errors.New("bad item")), false},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Errorf("retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestIsStatusRecord(t *testing.T) {
	rec := func(sk string) events.DynamoDBEventRecord {
		return events.DynamoDBEventRecord{Change: events.DynamoDBStreamRecord{Keys: map[string]events.DynamoDBAttributeValue{
			"roleName": events.NewStringAttribute("orders_writer"),
			"sk":       events.NewStringAttribute(sk),
		}}}
	}
	if !isStatusRecord(rec("STATUS#DEFINITION")) || !isStatusRecord(rec("STATUS#arn:aws:iam::1:role/x")) {
		t.Error("status records not recognised")
	}
	if isStatusRecord(rec("DEFINITION")) || isStatusRecord(rec("arn:aws:iam::1:role/x")) {
		t.Error("definition or mapping treated as a status record")
	}
}
