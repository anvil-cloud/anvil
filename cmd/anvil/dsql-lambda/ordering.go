package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ── Concurrent records ──────────────────────────────────────────────────────
//
// Items with different keys (a role, a table, an IAM mapping) can land on
// different stream shards, so their records run in parallel and in any order.
// Pulumi's DependsOn only orders the writes, not the processing. Two things
// make that safe:
//
//   1. Conflicts are retried here. DSQL rejects a DDL statement with SQLSTATE
//      40001 (OC000/OC001) when another transaction changed the catalog first.
//      Retrying with jittered backoff stops two records failing in lockstep,
//      which the stream's own back-to-back retries can't do.
//
//   2. Scoped grants don't depend on order. A role grants only on the scoped
//      tables that already exist; a table, once created, grants to every role
//      scoped to it. Whichever record runs second completes the grant. The role
//      path creates the role before checking tables, and the table path creates
//      the table before looking up roles, so one of them always sees both.

var (
	retryAttempts = 8
	retryBase     = 100 * time.Millisecond
	retryMax      = 2 * time.Second
	sleep         = func(ctx context.Context, d time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
			return nil
		}
	}
)

// isConflict reports whether err is DSQL's optimistic-concurrency rejection.
func isConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "40001"
}

// withRetry runs fn, retrying conflicts with jittered exponential backoff.
// Any other error is returned straight away.
func withRetry(ctx context.Context, what string, fn func() error) error {
	delay := retryBase
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !isConflict(err) || attempt == retryAttempts {
			return err
		}
		wait := delay/2 + time.Duration(rand.Int64N(int64(delay/2)+1))
		log.Printf("[dsql-bootstrap]   conflict on %s (attempt %d/%d) — retrying in %s", what, attempt, retryAttempts, wait)
		if err := sleep(ctx, wait); err != nil {
			return err
		}
		delay = min(delay*2, retryMax)
	}
}

// ensureSchema creates the schema only if it doesn't exist. CREATE SCHEMA IF
// NOT EXISTS is still DDL in DSQL, so running it when the schema is already
// there can make a concurrent CREATE TABLE fail with a conflict.
func ensureSchema(ctx context.Context, conn *pgx.Conn, schema string) error {
	var exists bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema,
	).Scan(&exists); err != nil {
		return fmt.Errorf("checking schema %q existence: %w", schema, err)
	}
	if exists {
		return nil
	}
	return execAll(ctx, conn, []string{fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", pgIdent(schema))})
}

// roleExists reports whether the Postgres role has been created.
func roleExists(ctx context.Context, conn *pgx.Conn, roleName string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", roleName,
	).Scan(&exists)
	return exists, err
}

// tablesInSchema returns the names of the tables that exist in schema.
func tablesInSchema(ctx context.Context, conn *pgx.Conn, schema string) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT tablename FROM pg_tables WHERE schemaname = $1", schema)
	if err != nil {
		return nil, fmt.Errorf("listing tables in schema %q: %w", schema, err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		present[name] = true
	}
	return present, rows.Err()
}

// roleGrantStatementsNow returns the role's grants that can run right now:
// schema-wide grants always can; scoped grants only on tables that exist. A
// scoped table that doesn't exist yet is granted by its own table record.
// granted lists the scoped tables included.
func roleGrantStatementsNow(ctx context.Context, conn *pgx.Conn, role roleConfig) (stmts, granted []string, err error) {
	if len(role.TableScoping) == 0 {
		return buildRoleGrantStatements(role), nil, nil
	}
	present, err := tablesInSchema(ctx, conn, role.Schema)
	if err != nil {
		return nil, nil, err
	}
	for _, t := range role.TableScoping {
		if present[t] {
			granted = append(granted, t)
		} else {
			log.Printf("[dsql-bootstrap]   table %s.%s doesn't exist yet — it is granted to %q when its table record runs",
				role.Schema, t, role.RoleName)
		}
	}
	return buildScopedGrantStatements(role, granted), granted, nil
}

type tableScanner interface {
	Scan(ctx context.Context, in *dynamodb.ScanInput, optFns ...func(*dynamodb.Options)) (*dynamodb.ScanOutput, error)
}

// rolesScopedTo returns the role definitions in the roles table whose
// tableScoping includes schema.table.
func rolesScopedTo(ctx context.Context, ddb tableScanner, schema, table string) ([]roleConfig, error) {
	var roles []roleConfig
	pages := dynamodb.NewScanPaginator(ddb, &dynamodb.ScanInput{
		TableName:        aws.String(rolesTableName),
		ConsistentRead:   aws.Bool(true),
		FilterExpression: aws.String("sk = :def AND #schema = :schema AND contains(tableScoping, :table)"),
		ExpressionAttributeNames: map[string]string{
			"#schema": "schema",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":def":    &types.AttributeValueMemberS{Value: "DEFINITION"},
			":schema": &types.AttributeValueMemberS{Value: schema},
			":table":  &types.AttributeValueMemberS{Value: table},
		},
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("scanning roles table for roles scoped to %s.%s: %w", schema, table, err)
		}
		for _, item := range page.Items {
			image, err := fromSDKItem(item)
			if err != nil {
				return nil, err
			}
			roles = append(roles, extractRoleConfig(image))
		}
	}
	return roles, nil
}

// grantScopedRolesOnTable grants a newly created table to every role scoped to
// it, and rewrites each role's status so it lists the table. Roles not created
// yet are skipped: their own record grants the table.
func grantScopedRolesOnTable(ctx context.Context, conn *pgx.Conn, ddb rolesTableAPI, def tableDef) error {
	roles, err := rolesScopedTo(ctx, ddb, def.Schema, def.Name)
	if err != nil {
		return err
	}
	for _, role := range roles {
		if err := validateRoleConfig(role); err != nil {
			log.Printf("[dsql-bootstrap]   REJECTED scoped role → %v", err)
			continue
		}
		exists, err := roleExists(ctx, conn, role.RoleName)
		if err != nil {
			return fmt.Errorf("checking role %q existence: %w", role.RoleName, err)
		}
		if !exists {
			log.Printf("[dsql-bootstrap]   role %q not created yet — its own record grants %s.%s",
				role.RoleName, def.Schema, def.Name)
			continue
		}
		log.Printf("[dsql-bootstrap]   granting %s.%s to scoped role %q", def.Schema, def.Name, role.RoleName)
		if err := execAll(ctx, conn, buildScopedGrantStatements(role, []string{def.Name})); err != nil {
			return err
		}
		if o, err := roleOutcome(ctx, conn, role); err == nil {
			writeStatus(ctx, ddb, role.RoleName, "DEFINITION", role.Version, o)
		}
	}
	return nil
}

// roleOutcome is a role's status, from the scoped tables that exist now. The
// role record and the table records that grant it can both write it; reading
// the catalog at write time keeps whichever writes last accurate.
func roleOutcome(ctx context.Context, conn *pgx.Conn, role roleConfig) (outcome, error) {
	var granted []string
	if len(role.TableScoping) > 0 {
		present, err := tablesInSchema(ctx, conn, role.Schema)
		if err != nil {
			return outcome{}, err
		}
		for _, t := range role.TableScoping {
			if present[t] {
				granted = append(granted, t)
			}
		}
	}
	return outcome{State: stateApplied, Message: describeGrants(role, granted)}, nil
}
