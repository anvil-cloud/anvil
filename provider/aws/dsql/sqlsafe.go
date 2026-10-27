package dsql

import (
	"regexp"
	"strings"
)

// Deploy-time copy of the bootstrap Lambda's SQL-safety allowlists
// (cmd/anvil/dsql-lambda/sqlsafe.go). Grants, column types and column defaults
// are SQL syntax rather than names, so they can't be quoted — they're checked
// against allowlists instead. The Lambda enforces; this copy fails `anvil
// deploy` early with a clear error. Keep the two in sync.

// dsqlAllowedGrants is the DSQLGrant enum.
var dsqlAllowedGrants = map[string]bool{
	"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
	"TRUNCATE": true, "REFERENCES": true,
}

// dsqlDefaultKeywords are the non-literal defaults allowed: constants and
// argument-free time/uuid functions. Matched case-insensitively.
var dsqlDefaultKeywords = map[string]bool{
	"true": true, "false": true, "null": true,
	"now()": true, "gen_random_uuid()": true,
	"current_timestamp": true, "current_date": true, "current_time": true,
	"localtimestamp": true, "localtime": true,
	"clock_timestamp()": true, "statement_timestamp()": true, "transaction_timestamp()": true,
}

var (
	dsqlDefaultNumber = regexp.MustCompile(`^-?\d+(\.\d+)?$`)
	// A single-quoted literal ('' is an escaped quote inside it), optionally
	// cast to a supported type: 'pending', 'it''s', '{}'::jsonb, '1 day'::interval.
	dsqlDefaultString = regexp.MustCompile(`^'(?:[^']|'')*'(?:::([a-z ]+))?$`)
)

// validDefault reports whether a column default is one of the allowed forms:
// a string literal (optionally cast), a number, or an entry in dsqlDefaultKeywords.
func validDefault(s string) bool {
	t := strings.TrimSpace(s)
	if dsqlDefaultKeywords[strings.ToLower(t)] || dsqlDefaultNumber.MatchString(t) {
		return true
	}
	m := dsqlDefaultString.FindStringSubmatch(t)
	return m != nil && (m[1] == "" || dsqlSupportedTypes[m[1]])
}
