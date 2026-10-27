package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// Mirrored in provider/aws/dsql/sqlsafe_test.go — keep the two lists in sync.
var (
	allowedDefaults = []string{
		"now()", "NOW()", "gen_random_uuid()", "current_timestamp", "CURRENT_DATE",
		"localtimestamp", "clock_timestamp()", "true", "FALSE", "null",
		"0", "-1", "3.14",
		"'pending'", "''", "'it''s'", "'a'''", "'{}'::jsonb", "'1 day'::interval", "'0'::double precision",
		" now() ",
	}
	rejectedDefaults = []string{
		"now(); DROP SCHEMA orders CASCADE",
		"'a'); DROP SCHEMA orders CASCADE; --",
		"'a' || current_user",
		"'unterminated",
		"'a''",
		"'x'::text; DROP TABLE t",
		"'x'::regclass",
		"(SELECT 1)",
		"pg_sleep(10)",
		"now() + interval '1 day'",
		"1e10",
		"E'\\x27'",
		"",
	}
)

func TestValidDefault(t *testing.T) {
	for _, s := range allowedDefaults {
		if !validDefault(s) {
			t.Errorf("rejected allowed default %q", s)
		}
	}
	for _, s := range rejectedDefaults {
		if validDefault(s) {
			t.Errorf("accepted unsafe default %q", s)
		}
	}
}

func TestValidateRoleConfig(t *testing.T) {
	ok := roleConfig{RoleName: "orders_api", Grants: []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES"}}
	if err := validateRoleConfig(ok); err != nil {
		t.Errorf("valid role: %v", err)
	}
	for _, grants := range [][]string{
		{"SELECT ON ALL TABLES IN SCHEMA public TO attacker; --"},
		{"ALL"},
		{"select"},
		{},
	} {
		if err := validateRoleConfig(roleConfig{RoleName: "r", Grants: grants}); err == nil {
			t.Errorf("accepted grants %q", grants)
		}
	}
}

func TestValidateTableDef(t *testing.T) {
	def := tableDef{Schema: "orders", Name: "orders", Columns: []columnDef{
		{Name: "id", Type: "uuid", Default: "gen_random_uuid()"},
		{Name: "total", Type: "double precision"},
	}}
	if err := validateTableDef(def); err != nil {
		t.Errorf("valid table: %v", err)
	}

	badType := def
	badType.Columns = []columnDef{{Name: "id", Type: "uuid PRIMARY KEY); DROP SCHEMA orders CASCADE; --"}}
	if err := validateTableDef(badType); err == nil {
		t.Error("accepted injected type")
	}

	badDefault := def
	badDefault.Columns = []columnDef{{Name: "id", Type: "uuid", Default: "gen_random_uuid()); DROP SCHEMA orders CASCADE; --"}}
	if err := validateTableDef(badDefault); err == nil {
		t.Error("accepted injected default")
	}
}

// A rejected record must return before any admin connection is attempted.
// DSQL_ENDPOINT is empty in tests, so reaching connectAdmin would error —
// a nil result means the record was dropped first.
func TestProcessRecordRejectsUnsafeItemsBeforeConnecting(t *testing.T) {
	ctx := context.Background()

	role := events.DynamoDBEventRecord{
		EventID:   "role",
		EventName: "INSERT",
		Change: events.DynamoDBStreamRecord{NewImage: map[string]events.DynamoDBAttributeValue{
			"roleName": events.NewStringAttribute("orders_api"),
			"sk":       events.NewStringAttribute("DEFINITION"),
			"schema":   events.NewStringAttribute("orders"),
			"grants": events.NewListAttribute([]events.DynamoDBAttributeValue{
				events.NewStringAttribute("SELECT ON ALL TABLES IN SCHEMA orders TO public; --"),
			}),
		}},
	}
	if o, err := processRecord(ctx, nil, role); err != nil || o.State != stateFailed {
		t.Errorf("role with injected grant: state=%q err=%v (want failed before connecting)", o.State, err)
	}

	defJSON, _ := json.Marshal(tableDef{Schema: "orders", Name: "orders", PrimaryKey: []string{"id"}, Columns: []columnDef{
		{Name: "id", Type: "uuid", Default: "now(); DROP SCHEMA orders CASCADE"},
	}})
	for _, event := range []string{"INSERT", "MODIFY"} {
		image := map[string]events.DynamoDBAttributeValue{
			"roleName":   events.NewStringAttribute("table#orders.orders"),
			"sk":         events.NewStringAttribute("DEFINITION"),
			"kind":       events.NewStringAttribute("table"),
			"definition": events.NewStringAttribute(string(defJSON)),
		}
		table := events.DynamoDBEventRecord{
			EventID:   "table",
			EventName: event,
			Change:    events.DynamoDBStreamRecord{NewImage: image, OldImage: image},
		}
		if o, err := processRecord(ctx, nil, table); err != nil || o.State != stateFailed {
			t.Errorf("%s table with injected default: state=%q err=%v (want failed before connecting)", event, o.State, err)
		}
	}
}
