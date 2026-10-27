package main

import (
	"fmt"
	"regexp"
	"strings"
)

// ── SQL-safety checks on item values ────────────────────────────────────────
//
// Identifiers (schema, role, table, column, index names) are always quoted with
// pgIdent, and the IAM ARN with pgLiteral. Three values can't be quoted because
// they are SQL syntax, not names: a role's grants, a column's type, and a
// column's default. Those are checked against allowlists here before any SQL
// is built, so whoever can write to the roles table still can't run arbitrary
// SQL as admin.
//
// The provider runs the same checks at deploy time (provider/aws/dsql/sqlsafe.go)
// for a clear error. Keep the two in sync; this copy is the one that enforces.

// allowedGrants is the DSQLGrant enum.
var allowedGrants = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"TRUNCATE": true, "REFERENCES": true,
}

// supportedTypes is the set of DSQL-supported column types.
var supportedTypes = map[string]bool{
	"uuid": true, "text": true, "varchar": true, "char": true,
	"boolean": true, "smallint": true, "integer": true, "bigint": true,
	"real": true, "double precision": true, "numeric": true,
	"date": true, "time": true, "timestamp": true, "timestamptz": true,
	"interval": true, "bytea": true, "json": true, "jsonb": true,
}

// defaultKeywords are the non-literal defaults allowed: constants and
// argument-free time/uuid functions. Matched case-insensitively.
var defaultKeywords = map[string]bool{
	"true": true, "false": true, "null": true,
	"now()": true, "gen_random_uuid()": true,
	"current_timestamp": true, "current_date": true, "current_time": true,
	"localtimestamp": true, "localtime": true,
	"clock_timestamp()": true, "statement_timestamp()": true, "transaction_timestamp()": true,
}

var (
	defaultNumber = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
	// A single-quoted literal ('' is an escaped quote inside it), optionally
	// cast to a supported type: 'pending', 'it''s', '{}'::jsonb, '1 day'::interval.
	defaultString = regexp.MustCompile(`^'(?:[^']|'')*'(?:::([a-z ]+))?$`)
)

// validDefault reports whether a column default is one of the allowed forms:
// a string literal (optionally cast), a number, or an entry in defaultKeywords.
func validDefault(s string) bool {
	t := strings.TrimSpace(s)
	if defaultKeywords[strings.ToLower(t)] || defaultNumber.MatchString(t) {
		return true
	}
	m := defaultString.FindStringSubmatch(t)
	return m != nil && (m[1] == "" || supportedTypes[m[1]])
}

func validateRoleConfig(role roleConfig) error {
	if len(role.Grants) == 0 {
		return fmt.Errorf("role %q has no grants", role.RoleName)
	}
	for _, g := range role.Grants {
		if !allowedGrants[g] {
			return fmt.Errorf("role %q: grant %q is not allowed", role.RoleName, g)
		}
	}
	return nil
}

func validateTableDef(def tableDef) error {
	for _, c := range def.Columns {
		if !supportedTypes[c.Type] {
			return fmt.Errorf("table %s.%s column %q: type %q is not allowed", def.Schema, def.Name, c.Name, c.Type)
		}
		if c.Default != "" && !validDefault(c.Default) {
			return fmt.Errorf("table %s.%s column %q: default %q is not allowed", def.Schema, def.Name, c.Name, c.Default)
		}
	}
	return nil
}
