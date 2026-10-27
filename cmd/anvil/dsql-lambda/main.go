package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	dsqlauth "github.com/aws/aws-sdk-go-v2/feature/dsql/auth"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ── NOTES FOR FRONTEND / DASHBOARD ──────────────────────────────────────────
//
// This Lambda's only trigger is a DynamoDB Stream — no lambda:InvokeFunction
// is granted to anything. Every record is re-checked against the table before
// it is acted on (verify.go), so a direct invoke can only replay what is
// already there.
//
// The cluster endpoint and region are set as Lambda environment variables
// (DSQL_ENDPOINT, DSQL_REGION) — they are NOT stored in the DynamoDB items.
// ROLES_TABLE_NAME and ROLES_STREAM_ARN identify the table records are
// verified against.
// Each DSQL component has its own table and Lambda, so multi-cluster
// deployments are fully isolated.
//
// DynamoDB table schema (composite key):
//
//   PK: roleName (S)   — Postgres role name, e.g. "app_role"
//   SK: sk (S)         — discriminator: "DEFINITION" or an IAM role ARN
//
// Two item types:
//
//   1. Role definition (sk = "DEFINITION"):
//      Written by Pulumi from the roles[] array on the DSQL component.
//      Attributes: schema (S), grants (L of S, e.g. ["SELECT", "INSERT"])
//
//      INSERT → CREATE SCHEMA, CREATE ROLE, GRANT USAGE, GRANT privileges,
//               ALTER DEFAULT PRIVILEGES
//      MODIFY → REVOKE old, GRANT new
//      REMOVE → REVOKE ALL, DROP OWNED BY, DROP ROLE
//
//   2. IAM mapping (sk = full IAM role ARN):
//      Written by DSQLConnect component (via dsql.grantConnect).
//      No extra attributes — roleName + ARN is all this Lambda needs.
//
//      INSERT → AWS IAM GRANT "roleName" TO 'arn:...'
//      REMOVE → AWS IAM REVOKE "roleName" FROM 'arn:...'
//
// The Lambda connects as admin using dsql:DbConnectAdmin IAM auth.
// Auth tokens are generated per-request — no long-lived credentials.
//
// Error handling: DSQL concurrency conflicts (40001) are retried in-process
// with jittered backoff, and scoped grants don't depend on record order
// (ordering.go). Any other SQL failure returns an error and the stream retries
// the record up to the event source mapping's retry limit.
//
// ─────────────────────────────────────────────────────────────────────────────

var (
	dsqlEndpoint = os.Getenv("DSQL_ENDPOINT")
	dsqlRegion   = os.Getenv("DSQL_REGION")
)

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context, event events.DynamoDBEvent) error {
	log.Printf("[dsql-bootstrap] received %d stream record(s) | endpoint=%s region=%s",
		len(event.Records), dsqlEndpoint, dsqlRegion)

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(dsqlRegion))
	if err != nil {
		return fmt.Errorf("loading AWS config: %w", err)
	}
	ddb := dynamodb.NewFromConfig(cfg)

	for _, record := range event.Records {
		// Status items are this Lambda's own reports; acting on them would loop.
		if isStatusRecord(record) {
			continue
		}
		ok, err := verifyRecord(ctx, ddb, &record)
		if err != nil {
			return fmt.Errorf("failed to verify record %s: %w", record.EventID, err)
		}
		if !ok {
			continue
		}
		if err := handleRecord(ctx, ddb, record); err != nil {
			return err
		}
	}

	log.Printf("[dsql-bootstrap] all %d record(s) processed", len(event.Records))
	return nil
}

// handleRecord processes one verified record and reports it in a status item.
// Only failures retrying can fix are returned, so the stream retries them; a
// permanent failure is recorded and the stream moves on.
func handleRecord(ctx context.Context, ddb rolesTableAPI, record events.DynamoDBEventRecord) error {
	pk := record.Change.Keys["roleName"].String()
	sk := record.Change.Keys["sk"].String()

	o, err := processRecord(ctx, ddb, record)
	switch {
	case record.EventName == "REMOVE":
		deleteStatus(ctx, ddb, pk, sk)
	case err != nil:
		writeStatus(ctx, ddb, pk, sk, attrString(record.Change.NewImage, "version"),
			outcome{State: stateFailed, Message: err.Error()})
	default:
		writeStatus(ctx, ddb, pk, sk, attrString(record.Change.NewImage, "version"), o)
	}
	if err != nil {
		log.Printf("[dsql-bootstrap] ERROR processing record %s: %v", record.EventID, err)
		if retryable(err) {
			return fmt.Errorf("failed to process record %s: %w", record.EventID, err)
		}
	}

	// Any change to roles or settings can leave a role the code no longer has.
	if isRoleDefinition(record) || pk == settingsKey {
		runSweep(ctx, ddb)
	}
	return nil
}

func recordImage(record events.DynamoDBEventRecord) map[string]events.DynamoDBAttributeValue {
	if record.Change.NewImage != nil {
		return record.Change.NewImage
	}
	return record.Change.OldImage
}

func isRoleDefinition(record events.DynamoDBEventRecord) bool {
	image := recordImage(record)
	return image["sk"].String() == "DEFINITION" && attrString(image, "kind") == ""
}

func processRecord(ctx context.Context, ddb rolesTableAPI, record events.DynamoDBEventRecord) (outcome, error) {
	// Dispatch order:
	//   kind == "table"    → table DDL (create/additive changes; no-op on removal)
	//   kind == "settings" → nothing here; the sweep reads it
	//   sk  starts "arn:"  → IAM GRANT/REVOKE mapping
	//   else               → role definition (sk == "DEFINITION")
	image := recordImage(record)

	if isTable(image) {
		log.Printf("[dsql-bootstrap] record %s | type=TABLE event=%s id=%q",
			record.EventID, record.EventName, image["roleName"].String())
		return processTable(ctx, ddb, record)
	}

	if attrString(image, "kind") == "settings" {
		log.Printf("[dsql-bootstrap] record %s | type=SETTINGS event=%s", record.EventID, record.EventName)
		return outcome{}, nil
	}

	if isIAMMapping(image) {
		log.Printf("[dsql-bootstrap] record %s | type=IAM_MAPPING event=%s roleName=%q sk=%q",
			record.EventID, record.EventName, image["roleName"].String(), image["sk"].String())
		return processIAMMapping(ctx, record)
	}

	log.Printf("[dsql-bootstrap] record %s | type=ROLE_DEFINITION event=%s roleName=%q schema=%q",
		record.EventID, record.EventName, image["roleName"].String(), image["schema"].String())

	// Grants go into SQL unquoted — reject anything off the allowlist before
	// connecting. The rejection is the item's status; the stream moves on.
	if record.EventName == "INSERT" || record.EventName == "MODIFY" {
		if err := validateRoleConfig(extractRoleConfig(record.Change.NewImage)); err != nil {
			log.Printf("[dsql-bootstrap]   REJECTED → %v", err)
			return outcome{State: stateFailed, Message: fmt.Sprintf("rejected: %v", err)}, nil
		}
	}

	switch record.EventName {
	case "INSERT":
		return handleInsert(ctx, ddb, record.Change.NewImage)
	case "MODIFY":
		return handleModify(ctx, ddb, record.Change.OldImage, record.Change.NewImage)
	case "REMOVE":
		return outcome{}, handleRemove(ctx, record.Change.OldImage)
	default:
		return outcome{}, nil
	}
}

// ── INSERT: create schema, role, and grants ─────────────────────────────────

func handleInsert(ctx context.Context, ddb tableScanner, newImage map[string]events.DynamoDBAttributeValue) (outcome, error) {
	role := extractRoleConfig(newImage)

	conn, err := connectAdmin(ctx)
	if err != nil {
		return outcome{}, fmt.Errorf("admin connect failed: %w", err)
	}
	defer conn.Close(ctx)

	if len(role.TableScoping) > 0 {
		log.Printf("[dsql-bootstrap] INSERT role %q in schema %q | grants %v | scoped to tables %v",
			role.RoleName, role.Schema, role.Grants, role.TableScoping)
	} else {
		log.Printf("[dsql-bootstrap] INSERT role %q in schema %q | grants %v | schema-wide",
			role.RoleName, role.Schema, role.Grants)
	}

	// Schema then the role, each only when absent. DSQL has no CREATE ROLE IF
	// NOT EXISTS, so check pg_roles first — makes INSERT re-runnable on
	// retries/redeploys without a "role already exists" error.
	if err := ensureSchema(ctx, conn, role.Schema); err != nil {
		return outcome{}, err
	}

	created, err := ensureRole(ctx, conn, role.RoleName)
	if err != nil {
		return outcome{}, err
	}
	if created {
		if err := reapplyMappings(ctx, conn, ddb, role.RoleName); err != nil {
			return outcome{}, err
		}
	}

	// Grants are re-runnable (GRANT is idempotent). The role must exist before
	// this check so a table created in parallel can grant to it (ordering.go).
	statements, _, err := roleGrantStatementsNow(ctx, conn, role)
	if err != nil {
		return outcome{}, err
	}
	if err := execAll(ctx, conn, statements); err != nil {
		return outcome{}, err
	}

	logRoleGrants(ctx, conn, role.Schema, role.RoleName)
	return roleOutcome(ctx, conn, role)
}

// ensureRole creates the role only if it doesn't already exist, and tags it as
// Anvil's (roles.go). created reports whether it was made now, in which case
// the caller re-applies the role's IAM mappings (mappings.go). DSQL/Postgres has no CREATE ROLE IF NOT EXISTS, and DSQL
// forbids DDL inside DO/PLpgSQL blocks, so this is a SELECT (its own txn)
// followed by a conditional CREATE ROLE (its own txn) — both satisfying DSQL's
// one-DDL-per-transaction rule.
func ensureRole(ctx context.Context, conn *pgx.Conn, roleName string) (created bool, err error) {
	exists, err := roleExists(ctx, conn, roleName)
	if err != nil {
		return false, fmt.Errorf("checking role %q existence: %w", roleName, err)
	}
	if exists {
		log.Printf("[dsql-bootstrap]   role %q already exists — skipping CREATE ROLE", roleName)
	} else if err := execAll(ctx, conn, []string{fmt.Sprintf("CREATE ROLE %s WITH LOGIN", pgIdent(roleName))}); err != nil {
		return false, err
	}
	return !exists, tagRole(ctx, conn, roleName)
}

// ── MODIFY: diff grants and update ──────────────────────────────────────────

func handleModify(ctx context.Context, ddb tableScanner, oldImage, newImage map[string]events.DynamoDBAttributeValue) (outcome, error) {
	oldRole := extractRoleConfig(oldImage)
	newRole := extractRoleConfig(newImage)

	conn, err := connectAdmin(ctx)
	if err != nil {
		return outcome{}, fmt.Errorf("admin connect failed: %w", err)
	}
	defer conn.Close(ctx)

	// Revoke-all-then-re-grant: reset everything the role had on the old schema,
	// then apply the new spec (schema-wide or table-scoped). Safe and simple —
	// no per-privilege diffing. Runs only at deploy time, not on app traffic.
	//
	// Phase 1 — revoke old privileges, BEST-EFFORT. The old schema may no longer
	// exist (e.g. it was renamed or dropped), in which case the REVOKE fails with
	// 3F000 and there is simply nothing left to revoke. These must NOT abort the
	// re-grant below: doing so would leave the role with zero privileges on the
	// new schema (the exact failure mode where a schema rename strands a role).
	revokes := []string{
		fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA %s FROM %s",
			pgIdent(oldRole.Schema), pgIdent(oldRole.RoleName)),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s REVOKE ALL ON TABLES FROM %s",
			pgIdent(oldRole.Schema), pgIdent(oldRole.RoleName)),
	}
	if oldRole.Schema != newRole.Schema {
		revokes = append(revokes,
			fmt.Sprintf("REVOKE USAGE ON SCHEMA %s FROM %s", pgIdent(oldRole.Schema), pgIdent(oldRole.RoleName)))
	}
	execBestEffort(ctx, conn, revokes)

	// Phase 2 — apply the new spec. These MUST succeed. Scoped tables that don't
	// exist yet are skipped; their table record grants them (ordering.go).
	if err := ensureSchema(ctx, conn, newRole.Schema); err != nil {
		return outcome{}, err
	}
	created, err := ensureRole(ctx, conn, newRole.RoleName)
	if err != nil {
		return outcome{}, err
	}
	if created {
		if err := reapplyMappings(ctx, conn, ddb, newRole.RoleName); err != nil {
			return outcome{}, err
		}
	}
	statements, _, err := roleGrantStatementsNow(ctx, conn, newRole)
	if err != nil {
		return outcome{}, err
	}
	if err := execAll(ctx, conn, statements); err != nil {
		return outcome{}, err
	}
	return roleOutcome(ctx, conn, newRole)
}

// ── REMOVE: revoke everything and drop role ─────────────────────────────────
//
// dropRoleCompletely (roles.go) revokes the role's IAM mappings first — DSQL
// won't drop a role that still has one — then its privileges in every schema.
//
// A drop that fails is logged, not returned: a REMOVE that errored would block
// (poison) the stream behind it. The role's access is already revoked by then,
// and the sweep that runs after this record retries the drop and reports it.
//
// Note: DSQL does not support DROP OWNED BY (SQLSTATE 0A000). It isn't needed —
// app roles own no objects (the bootstrap Lambda creates tables as admin), so
// DROP ROLE after the revokes is sufficient.
func handleRemove(ctx context.Context, oldImage map[string]events.DynamoDBAttributeValue) error {
	role := extractRoleConfig(oldImage)

	conn, err := connectAdmin(ctx)
	if err != nil {
		// Connection failure IS worth retrying (transient) — return the error.
		return fmt.Errorf("admin connect failed: %w", err)
	}
	defer conn.Close(ctx)

	if err := dropRoleCompletely(ctx, conn, role.RoleName); err != nil {
		log.Printf("[dsql-bootstrap]   drop of role %q failed (the sweep retries it): %v", role.RoleName, err)
	}
	return nil
}

// ── IAM mapping items (sk = IAM role ARN) ───────────────────────────────────

func isIAMMapping(image map[string]events.DynamoDBAttributeValue) bool {
	sk := image["sk"].String()
	return strings.HasPrefix(sk, "arn:")
}

func processIAMMapping(ctx context.Context, record events.DynamoDBEventRecord) (outcome, error) {
	switch record.EventName {
	// MODIFY only happens when the item's attributes change (e.g. it gains a
	// version on upgrade); the mapping it describes is the same, so it is
	// re-applied. AWS IAM GRANT of an existing mapping is a no-op notice.
	case "INSERT", "MODIFY":
		roleName := record.Change.NewImage["roleName"].String()
		iamArn := record.Change.NewImage["sk"].String()

		log.Printf("[dsql-bootstrap] %s IAM mapping: db role %q ← IAM %s", record.EventName, roleName, iamArn)

		// The ARN lands inside a SQL string literal. Reject anything that isn't
		// a plain IAM role ARN before connecting.
		if !iamRoleArnPattern.MatchString(iamArn) {
			log.Printf("[dsql-bootstrap]   REJECTED → %q is not a valid IAM role ARN", iamArn)
			return outcome{State: stateFailed, Message: fmt.Sprintf("rejected: %q is not a valid IAM role ARN", iamArn)}, nil
		}

		conn, err := connectAdmin(ctx)
		if err != nil {
			return outcome{}, fmt.Errorf("admin connect failed: %w", err)
		}
		defer conn.Close(ctx)

		exists, err := roleExists(ctx, conn, roleName)
		if err != nil {
			return outcome{}, err
		}
		if !exists {
			return outcome{}, permanent(fmt.Errorf("database role %q doesn't exist, so nothing can connect as it", roleName))
		}

		sql := fmt.Sprintf("AWS IAM GRANT %s TO %s", pgIdent(roleName), pgLiteral(iamArn))
		log.Printf("[dsql-bootstrap]   SQL → %s", sql)
		if err := withRetry(ctx, "AWS IAM GRANT", func() error {
			_, err := conn.Exec(ctx, sql)
			return err
		}); err != nil {
			log.Printf("[dsql-bootstrap]   SQL FAILED → %s | %v", sql, err)
			return outcome{}, fmt.Errorf("SQL failed [%s]: %w", sql, err)
		}
		log.Printf("[dsql-bootstrap]   SQL OK → AWS IAM GRANT")

		// Confirm the mapping is actually present in DSQL's catalog.
		logIAMMappings(ctx, conn, roleName)
		return outcome{State: stateApplied, Message: fmt.Sprintf("%s can connect as %s", iamRoleName(iamArn), roleName)}, nil

	case "REMOVE":
		roleName := record.Change.OldImage["roleName"].String()
		iamArn := record.Change.OldImage["sk"].String()

		log.Printf("[dsql-bootstrap] REMOVE IAM mapping: db role %q ✗ IAM %s", roleName, iamArn)

		if !iamRoleArnPattern.MatchString(iamArn) {
			log.Printf("[dsql-bootstrap]   REJECTED → %q is not a valid IAM role ARN", iamArn)
			return outcome{}, nil
		}

		conn, err := connectAdmin(ctx)
		if err != nil {
			return outcome{}, fmt.Errorf("admin connect failed: %w", err)
		}
		defer conn.Close(ctx)

		// Best-effort: a failed REVOKE must not poison the stream. If the role
		// was already dropped, the mapping is gone anyway.
		execBestEffort(ctx, conn, []string{
			fmt.Sprintf("AWS IAM REVOKE %s FROM %s", pgIdent(roleName), pgLiteral(iamArn)),
		})
		return outcome{}, nil

	default:
		return outcome{}, nil
	}
}

// iamRoleName is the name part of an IAM role ARN, for status messages.
func iamRoleName(arn string) string {
	return arn[strings.LastIndex(arn, "/")+1:]
}

// ── Table items (kind = "table") ────────────────────────────────────────────
//
// Additive-only. The full table definition is stored as a JSON string in the
// "definition" attribute (mirrors provider/aws/dsql tableDefPayload).
//
//   INSERT / MODIFY → create the table if it's missing, otherwise add the
//                     columns and indexes the catalog lacks (tables.go).
//                     Removed columns/indexes and type changes are left alone
//                     (destructive changes are human-owned) and reported.
//   REMOVE          → no-op (the table stays in DSQL; only Pulumi state drops it)

type columnDef struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Length    int    `json:"length,omitempty"`
	Precision int    `json:"precision,omitempty"`
	Scale     int    `json:"scale,omitempty"`
	NotNull   bool   `json:"notNull,omitempty"`
	Unique    bool   `json:"unique,omitempty"`
	Default   string `json:"default,omitempty"`
}

type indexDef struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
}

type tableDef struct {
	Schema     string      `json:"schema"`
	Name       string      `json:"name"`
	PrimaryKey []string    `json:"primaryKey"`
	Columns    []columnDef `json:"columns"`
	Indexes    []indexDef  `json:"indexes,omitempty"`
}

func isTable(image map[string]events.DynamoDBAttributeValue) bool {
	if kind, ok := image["kind"]; ok {
		return kind.String() == "table"
	}
	return false
}

func parseTableDef(image map[string]events.DynamoDBAttributeValue) (tableDef, error) {
	var def tableDef
	raw := image["definition"].String()
	if raw == "" {
		return def, fmt.Errorf("table item missing definition attribute")
	}
	if err := json.Unmarshal([]byte(raw), &def); err != nil {
		return def, fmt.Errorf("parsing table definition: %w", err)
	}
	return def, nil
}

func processTable(ctx context.Context, ddb rolesTableAPI, record events.DynamoDBEventRecord) (outcome, error) {
	switch record.EventName {
	case "INSERT", "MODIFY":
		def, err := parseTableDef(record.Change.NewImage)
		if err != nil {
			return outcome{}, permanent(err)
		}
		if err := validateTableDef(def); err != nil {
			log.Printf("[dsql-bootstrap]   REJECTED → %v", err)
			return outcome{State: stateFailed, Message: fmt.Sprintf("rejected: %v", err)}, nil
		}
		return reconcileTable(ctx, ddb, def, record.EventName == "INSERT")

	case "REMOVE":
		// Destructive — Anvil never drops tables. The table stays in DSQL;
		// only the Pulumi state / DynamoDB item is removed.
		def, _ := parseTableDef(record.Change.OldImage)
		log.Printf("[dsql-bootstrap] REMOVE table %s.%s — no-op (destructive changes are human-owned; table left intact)",
			def.Schema, def.Name)
		return outcome{}, nil

	default:
		return outcome{}, nil
	}
}

// createTable creates the schema, the table, and its indexes (async).
func createTable(ctx context.Context, conn *pgx.Conn, def tableDef) error {
	log.Printf("[dsql-bootstrap] CREATE table %s.%s (%d columns, pk=%v, %d indexes)",
		def.Schema, def.Name, len(def.Columns), def.PrimaryKey, len(def.Indexes))

	// Schema first (its own transaction), then table (its own transaction).
	if err := ensureSchema(ctx, conn, def.Schema); err != nil {
		return err
	}
	if err := execAll(ctx, conn, []string{buildCreateTable(def)}); err != nil {
		return err
	}

	// Declared indexes and column-level unique indexes — each async.
	for _, idx := range tableIndexes(def) {
		if err := createIndexAsync(ctx, conn, def, idx); err != nil {
			return err
		}
	}
	return nil
}

// buildCreateTable composes the CREATE TABLE statement.
func buildCreateTable(def tableDef) string {
	parts := make([]string, 0, len(def.Columns)+1)
	for _, c := range def.Columns {
		parts = append(parts, buildColumnDDL(c))
	}
	pkCols := make([]string, len(def.PrimaryKey))
	for i, pk := range def.PrimaryKey {
		pkCols[i] = pgIdent(pk)
	}
	parts = append(parts, fmt.Sprintf("PRIMARY KEY (%s)", strings.Join(pkCols, ", ")))

	return fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s.%s (%s)",
		pgIdent(def.Schema), pgIdent(def.Name), strings.Join(parts, ", "))
}

// buildColumnDDL composes one column's DDL fragment (no PK — that's table-level).
func buildColumnDDL(c columnDef) string {
	typeStr := c.Type
	switch c.Type {
	case "varchar", "char":
		if c.Length > 0 {
			typeStr = fmt.Sprintf("%s(%d)", c.Type, c.Length)
		}
	case "numeric":
		if c.Precision > 0 {
			if c.Scale > 0 {
				typeStr = fmt.Sprintf("numeric(%d,%d)", c.Precision, c.Scale)
			} else {
				typeStr = fmt.Sprintf("numeric(%d)", c.Precision)
			}
		}
	}

	ddl := fmt.Sprintf("%s %s", pgIdent(c.Name), typeStr)
	if c.NotNull {
		ddl += " NOT NULL"
	}
	if c.Default != "" {
		ddl += " DEFAULT " + c.Default // unquoted; checked by validDefault first
	}
	return ddl
}

// createIndexAsync fires CREATE INDEX ASYNC and logs the job_id (fire-and-log,
// no polling). DSQL requires the ASYNC variant; the build runs in the background.
func createIndexAsync(ctx context.Context, conn *pgx.Conn, def tableDef, idx indexDef) error {
	cols := make([]string, len(idx.Columns))
	for i, c := range idx.Columns {
		cols[i] = pgIdent(c)
	}
	unique := ""
	if idx.Unique {
		unique = "UNIQUE "
	}
	sql := fmt.Sprintf("CREATE %sINDEX ASYNC IF NOT EXISTS %s ON %s.%s (%s)",
		unique, pgIdent(idx.Name), pgIdent(def.Schema), pgIdent(def.Name), strings.Join(cols, ", "))

	log.Printf("[dsql-bootstrap]   SQL → %s", sql)
	var jobID string
	// CREATE INDEX ASYNC returns a job_id row.
	if err := withRetry(ctx, "CREATE INDEX "+idx.Name, func() error {
		return conn.QueryRow(ctx, sql).Scan(&jobID)
	}); err != nil {
		// Some drivers return no row for IF NOT EXISTS no-op; treat as non-fatal
		// only if it's a "no rows" case, otherwise surface it.
		if err.Error() == "no rows in result set" {
			log.Printf("[dsql-bootstrap]   index %q already exists or no job returned", idx.Name)
			return nil
		}
		log.Printf("[dsql-bootstrap]   SQL FAILED → %s | %v", sql, err)
		return fmt.Errorf("creating index %s: %w", idx.Name, err)
	}
	log.Printf("[dsql-bootstrap]   index %q building async (job_id=%s) — monitor via sys.jobs", idx.Name, jobID)
	return nil
}

// ── Admin connection via IAM auth token ─────────────────────────────────────

func connectAdmin(ctx context.Context) (*pgx.Conn, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(dsqlRegion))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	token, err := dsqlauth.GenerateDBConnectAdminAuthToken(ctx, dsqlEndpoint, dsqlRegion, cfg.Credentials)
	if err != nil {
		return nil, fmt.Errorf("generating admin auth token: %w", err)
	}

	// Build the connection config WITHOUT the password — the DSQL auth token
	// is a presigned URL containing '&', '=', '?' which break libpq connection
	// string parsing. Set it on the Password field directly after parsing.
	connStr := fmt.Sprintf(
		"host=%s port=5432 user=admin dbname=postgres sslmode=verify-full",
		dsqlEndpoint,
	)

	connConfig, err := pgx.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parsing connection config: %w", err)
	}
	connConfig.Password = token

	log.Printf("[dsql-bootstrap] connecting as admin to %s (sslmode=verify-full)", dsqlEndpoint)
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		log.Printf("[dsql-bootstrap] admin connection FAILED: %v", err)
		return nil, fmt.Errorf("connecting to DSQL: %w", err)
	}
	log.Printf("[dsql-bootstrap] admin connection established")

	return conn, nil
}

// ── Helpers ─────────────────────────────────────────────────────────────────

type roleConfig struct {
	RoleName string
	Schema   string
	Grants   []string
	// TableScoping, when non-empty, narrows grants to these specific tables
	// instead of the whole schema.
	TableScoping []string
	// Version is the item's version, for its status item.
	Version string
}

func extractRoleConfig(image map[string]events.DynamoDBAttributeValue) roleConfig {
	r := roleConfig{
		RoleName: image["roleName"].String(),
		Schema:   image["schema"].String(),
		Version:  attrString(image, "version"),
	}

	if grantsAttr, ok := image["grants"]; ok {
		for _, item := range grantsAttr.List() {
			r.Grants = append(r.Grants, item.String())
		}
	}
	if scopeAttr, ok := image["tableScoping"]; ok {
		for _, item := range scopeAttr.List() {
			r.TableScoping = append(r.TableScoping, item.String())
		}
	}

	return r
}

// buildRoleGrantStatements returns the GRANT statements for a role.
//   - schema-wide (no TableScoping): GRANT ON ALL TABLES + ALTER DEFAULT
//     PRIVILEGES (covers current + future tables).
//   - table-scoped: GRANT on each named table only (no ALTER DEFAULT
//     PRIVILEGES — scoped roles do NOT auto-cover future tables).
// USAGE on the schema is always granted.
func buildRoleGrantStatements(role roleConfig) []string {
	grants := strings.Join(role.Grants, ", ")
	stmts := []string{
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", pgIdent(role.Schema), pgIdent(role.RoleName)),
	}

	if len(role.TableScoping) > 0 {
		return buildScopedGrantStatements(role, role.TableScoping)
	}

	// Schema-wide: current + future tables.
	stmts = append(stmts,
		fmt.Sprintf("GRANT %s ON ALL TABLES IN SCHEMA %s TO %s",
			grants, pgIdent(role.Schema), pgIdent(role.RoleName)),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT %s ON TABLES TO %s",
			pgIdent(role.Schema), grants, pgIdent(role.RoleName)),
	)
	return stmts
}

// buildScopedGrantStatements returns USAGE on the schema plus the role's grants
// on each of tables. With no tables it grants USAGE only — never schema-wide.
func buildScopedGrantStatements(role roleConfig, tables []string) []string {
	grants := strings.Join(role.Grants, ", ")
	stmts := []string{
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", pgIdent(role.Schema), pgIdent(role.RoleName)),
	}
	for _, table := range tables {
		stmts = append(stmts, fmt.Sprintf("GRANT %s ON %s.%s TO %s",
			grants, pgIdent(role.Schema), pgIdent(table), pgIdent(role.RoleName)))
	}
	return stmts
}

// execBestEffort runs cleanup statements, logging failures but never returning
// an error. Used by REMOVE paths so destructive cleanup can't poison the stream.
func execBestEffort(ctx context.Context, conn *pgx.Conn, statements []string) {
	for _, sql := range statements {
		log.Printf("[dsql-bootstrap]   SQL → %s", sql)
		tag, err := conn.Exec(ctx, sql)
		if err != nil {
			log.Printf("[dsql-bootstrap]   SQL FAILED (ignored, best-effort) → %s | %v", sql, err)
			continue
		}
		log.Printf("[dsql-bootstrap]   SQL OK → %s (%s)", sql, tag.String())
	}
}

func execAll(ctx context.Context, conn *pgx.Conn, statements []string) error {
	for _, sql := range statements {
		log.Printf("[dsql-bootstrap]   SQL → %s", sql)
		var tag pgconn.CommandTag
		err := withRetry(ctx, sql, func() error {
			var err error
			tag, err = conn.Exec(ctx, sql)
			return err
		})
		if err != nil {
			log.Printf("[dsql-bootstrap]   SQL FAILED → %s | %v", sql, err)
			return fmt.Errorf("SQL failed [%s]: %w", sql, err)
		}
		log.Printf("[dsql-bootstrap]   SQL OK → %s (%s)", sql, tag.String())
	}
	return nil
}

// logRoleGrants queries DSQL's catalog to show the privileges actually held by
// the role on the schema's tables. Purely diagnostic — surfaces in CloudWatch
// so you can confirm grants landed without connecting manually.
func logRoleGrants(ctx context.Context, conn *pgx.Conn, schema, roleName string) {
	// Schema-level USAGE.
	var hasUsage bool
	if err := conn.QueryRow(ctx,
		"SELECT has_schema_privilege($1, $2, 'USAGE')", roleName, schema,
	).Scan(&hasUsage); err != nil {
		log.Printf("[dsql-bootstrap]   verify: could not check schema USAGE: %v", err)
	} else {
		log.Printf("[dsql-bootstrap]   verify: role %q USAGE on schema %q = %v", roleName, schema, hasUsage)
	}

	// Table-level privileges granted to the role in this schema.
	rows, err := conn.Query(ctx, `
		SELECT table_name, privilege_type
		FROM information_schema.role_table_grants
		WHERE grantee = $1 AND table_schema = $2
		ORDER BY table_name, privilege_type`, roleName, schema)
	if err != nil {
		log.Printf("[dsql-bootstrap]   verify: could not list table grants: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var table, priv string
		if err := rows.Scan(&table, &priv); err != nil {
			continue
		}
		log.Printf("[dsql-bootstrap]   verify: grant %s.%s → %s on %q", schema, table, priv, roleName)
		count++
	}
	if count == 0 {
		log.Printf("[dsql-bootstrap]   verify: no existing table grants for %q in %q (expected if schema has no tables yet)", roleName, schema)
	}
}

// logIAMMappings queries DSQL's sys.iam_pg_role_mappings to show which IAM ARNs
// are mapped to which database roles. Confirms grantConnect / AWS IAM GRANT worked.
func logIAMMappings(ctx context.Context, conn *pgx.Conn, roleName string) {
	rows, err := conn.Query(ctx,
		"SELECT arn, pg_role_name FROM sys.iam_pg_role_mappings WHERE pg_role_name = $1", roleName)
	if err != nil {
		log.Printf("[dsql-bootstrap]   verify: could not query sys.iam_pg_role_mappings: %v", err)
		return
	}
	defer rows.Close()

	count := 0
	for rows.Next() {
		var arn, pgRole string
		if err := rows.Scan(&arn, &pgRole); err != nil {
			continue
		}
		log.Printf("[dsql-bootstrap]   verify: IAM mapping %s → db role %q", arn, pgRole)
		count++
	}
	if count == 0 {
		log.Printf("[dsql-bootstrap]   verify: NO IAM mappings found for db role %q — Lambdas cannot connect as this role yet", roleName)
	}
}

// pgIdent quotes a PostgreSQL identifier to prevent injection.
func pgIdent(name string) string {
	escaped := strings.ReplaceAll(name, `"`, `""`)
	return `"` + escaped + `"`
}

// pgLiteral quotes a PostgreSQL string literal to prevent injection.
func pgLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// iamRoleArnPattern matches an IAM role ARN in any partition. The path/name
// charset is IAM's own ([\w+=,.@-] plus '/'), so it can never contain a quote.
var iamRoleArnPattern = regexp.MustCompile(`^arn:aws[a-z-]*:iam::\d{12}:role/[\w+=,.@/-]+$`)
