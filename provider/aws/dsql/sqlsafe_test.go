package dsql

import (
	"strings"
	"testing"
)

// Mirrored in cmd/anvil/dsql-lambda/sqlsafe_test.go — keep the two lists in sync.
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

func testSchemas() []DSQLSchemaArgs {
	return []DSQLSchemaArgs{{
		Name:  "orders",
		Roles: []DSQLRoleArgs{{Name: "orders_api", Grants: []string{"SELECT", "INSERT"}}},
		Tables: []DSQLTableArgs{{
			Name:       "orders",
			PrimaryKey: []string{"id"},
			Columns: []DSQLColumnArgs{
				{Name: "id", Type: "uuid", Default: "gen_random_uuid()"},
				{Name: "status", Type: "text", Default: "'pending'"},
			},
		}},
	}}
}

func TestValidateSchemasRejectsUnsafeSQL(t *testing.T) {
	if err := validateSchemas(testSchemas()); err != nil {
		t.Fatalf("valid schemas: %v", err)
	}

	badGrant := testSchemas()
	badGrant[0].Roles[0].Grants = []string{"SELECT ON ALL TABLES IN SCHEMA orders TO public; --"}
	if err := validateSchemas(badGrant); err == nil || !strings.Contains(err.Error(), "is not one of SELECT") {
		t.Errorf("injected grant: %v", err)
	}

	badDefault := testSchemas()
	badDefault[0].Tables[0].Columns[1].Default = "'pending'); DROP SCHEMA orders CASCADE; --"
	if err := validateSchemas(badDefault); err == nil || !strings.Contains(err.Error(), "is not allowed") {
		t.Errorf("injected default: %v", err)
	}
}
