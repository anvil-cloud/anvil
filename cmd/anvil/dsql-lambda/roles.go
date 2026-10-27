package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/jackc/pgx/v5"
)

// ── Role cleanup ────────────────────────────────────────────────────────────
//
// Every role Anvil creates is tagged with a COMMENT (roleTag), so Anvil can
// tell its roles apart from DSQL's built-in ones and from roles made by hand.
//
// After any role or settings record, the sweep compares the database's roles
// with the role items in the table:
//
//   - tagged, not in the table   → dropped (Anvil made it; it's gone from code)
//   - untagged, not in the table → reported in the settings status, or dropped
//                                  when the component sets pruneUnmanagedRoles
//   - admin, dbowner, superusers → never touched
//
// Dropping a role first revokes its IAM mappings and its privileges in every
// schema; a role with a mapping can't be dropped (SQLSTATE 2BP01).

const roleTag = "managed-by:anvil"

// builtinRoles are DSQL's own roles.
var builtinRoles = map[string]bool{"admin": true, "dbowner": true}

type dbRole struct {
	Name   string
	Super  bool
	Tagged bool
}

// classifyRoles splits the database's roles that aren't declared in code into
// those to drop and those to report.
func classifyRoles(roles []dbRole, declared map[string]bool, prune bool) (drop, unmanaged []string) {
	for _, r := range roles {
		if r.Super || builtinRoles[r.Name] || strings.HasPrefix(r.Name, "pg_") || declared[r.Name] {
			continue
		}
		if r.Tagged || prune {
			drop = append(drop, r.Name)
		} else {
			unmanaged = append(unmanaged, r.Name)
		}
	}
	sort.Strings(drop)
	sort.Strings(unmanaged)
	return drop, unmanaged
}

// tagRole marks a role as Anvil's. A declared role that already exists
// untagged (made by hand, or by an older Anvil) is adopted.
func tagRole(ctx context.Context, conn *pgx.Conn, roleName string) error {
	var comment *string
	if err := conn.QueryRow(ctx,
		"SELECT shobj_description(oid, 'pg_authid') FROM pg_roles WHERE rolname = $1", roleName,
	).Scan(&comment); err != nil {
		return fmt.Errorf("reading role %q comment: %w", roleName, err)
	}
	if comment != nil && *comment == roleTag {
		return nil
	}
	return execAll(ctx, conn, []string{
		fmt.Sprintf("COMMENT ON ROLE %s IS %s", pgIdent(roleName), pgLiteral(roleTag)),
	})
}

// userSchemas lists the schemas a role could hold privileges in.
func userSchemas(ctx context.Context, conn *pgx.Conn) ([]string, error) {
	rows, err := conn.Query(ctx,
		`SELECT nspname FROM pg_namespace
		 WHERE nspname NOT LIKE 'pg\_%' AND nspname NOT IN ('information_schema', 'sys')
		 ORDER BY nspname`)
	if err != nil {
		return nil, fmt.Errorf("listing schemas: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// dropRoleCompletely revokes the role's IAM mappings and privileges, then
// drops it. Revokes are best-effort; the drop's error is returned.
func dropRoleCompletely(ctx context.Context, conn *pgx.Conn, roleName string) error {
	log.Printf("[dsql-bootstrap] dropping role %q (mappings, privileges, role)", roleName)

	rows, err := conn.Query(ctx, "SELECT arn FROM sys.iam_pg_role_mappings WHERE pg_role_name = $1", roleName)
	if err != nil {
		return fmt.Errorf("listing IAM mappings for %q: %w", roleName, err)
	}
	var arns []string
	for rows.Next() {
		var arn string
		if err := rows.Scan(&arn); err == nil {
			arns = append(arns, arn)
		}
	}
	rows.Close()

	var stmts []string
	for _, arn := range arns {
		stmts = append(stmts, fmt.Sprintf("AWS IAM REVOKE %s FROM %s", pgIdent(roleName), pgLiteral(arn)))
	}
	schemas, err := userSchemas(ctx, conn)
	if err != nil {
		return err
	}
	for _, s := range schemas {
		stmts = append(stmts,
			fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA %s FROM %s", pgIdent(s), pgIdent(roleName)),
			fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA %s REVOKE ALL ON TABLES FROM %s", pgIdent(s), pgIdent(roleName)),
			fmt.Sprintf("REVOKE USAGE ON SCHEMA %s FROM %s", pgIdent(s), pgIdent(roleName)),
		)
	}
	execBestEffort(ctx, conn, stmts)

	return execAll(ctx, conn, []string{fmt.Sprintf("DROP ROLE IF EXISTS %s", pgIdent(roleName))})
}

type tableContents struct {
	declared        map[string]bool // role names with a DEFINITION item
	prune           bool
	settingsVersion string
}

// readTableContents scans the roles table for role definitions and settings.
func readTableContents(ctx context.Context, ddb tableScanner) (tableContents, error) {
	tc := tableContents{declared: map[string]bool{}}
	pages := dynamodb.NewScanPaginator(ddb, &dynamodb.ScanInput{
		TableName:      aws.String(rolesTableName),
		ConsistentRead: aws.Bool(true),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return tc, fmt.Errorf("scanning roles table: %w", err)
		}
		for _, item := range page.Items {
			image, err := fromSDKItem(item)
			if err != nil {
				return tc, err
			}
			pk, sk := image["roleName"].String(), image["sk"].String()
			switch {
			case sk != "DEFINITION":
			case pk == settingsKey:
				tc.settingsVersion = attrString(image, "version")
				if v, ok := image["pruneUnmanagedRoles"]; ok && v.DataType() == events.DataTypeBoolean {
					tc.prune = v.Boolean()
				}
			case attrString(image, "kind") == "":
				tc.declared[pk] = true
			}
		}
	}
	return tc, nil
}

// runSweep drops stray roles and reports unmanaged ones. Its result is the
// settings item's status.
func runSweep(ctx context.Context, ddb rolesTableAPI) {
	tc, err := readTableContents(ctx, ddb)
	if err != nil {
		log.Printf("[dsql-bootstrap] sweep skipped: %v", err)
		return
	}
	o, err := sweepRoles(ctx, tc)
	if err != nil {
		o = outcome{State: stateFailed, Message: fmt.Sprintf("role cleanup failed: %v", err)}
	}
	writeStatus(ctx, ddb, settingsKey, "DEFINITION", tc.settingsVersion, o)
}

func sweepRoles(ctx context.Context, tc tableContents) (outcome, error) {
	conn, err := connectAdmin(ctx)
	if err != nil {
		return outcome{}, err
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, `
		SELECT rolname, rolsuper, coalesce(shobj_description(oid, 'pg_authid'), '') = $1
		FROM pg_roles`, roleTag)
	if err != nil {
		return outcome{}, fmt.Errorf("listing roles: %w", err)
	}
	var roles []dbRole
	for rows.Next() {
		var r dbRole
		if err := rows.Scan(&r.Name, &r.Super, &r.Tagged); err != nil {
			rows.Close()
			return outcome{}, err
		}
		roles = append(roles, r)
	}
	rows.Close()

	drop, unmanaged := classifyRoles(roles, tc.declared, tc.prune)
	var dropped, failed []string
	for _, name := range drop {
		if err := dropRoleCompletely(ctx, conn, name); err != nil {
			failed = append(failed, fmt.Sprintf("%s (%v)", name, err))
		} else {
			dropped = append(dropped, name)
		}
	}

	var parts []string
	if len(dropped) > 0 {
		parts = append(parts, "removed roles no longer in code: "+strings.Join(dropped, ", "))
	}
	if len(failed) > 0 {
		parts = append(parts, "couldn't remove: "+strings.Join(failed, "; "))
	}
	if len(unmanaged) > 0 {
		parts = append(parts, "roles in the database but not in your code, not created by Anvil: "+
			strings.Join(unmanaged, ", ")+" — set pruneUnmanagedRoles: true to remove them")
	}
	switch {
	case len(failed) > 0:
		return outcome{State: stateFailed, Message: strings.Join(parts, "; ")}, nil
	case len(unmanaged) > 0:
		return outcome{State: stateDrift, Message: strings.Join(parts, "; ")}, nil
	case len(dropped) > 0:
		return outcome{State: stateApplied, Message: strings.Join(parts, "; ")}, nil
	default:
		return outcome{State: stateApplied, Message: "every role matches your code"}, nil
	}
}

// describeGrants is a role's status message: what it was granted.
func describeGrants(role roleConfig, grantedTables []string) string {
	grants := strings.Join(role.Grants, ", ")
	if len(role.TableScoping) == 0 {
		return fmt.Sprintf("%s on every table in %s", grants, role.Schema)
	}
	if len(grantedTables) == 0 {
		return fmt.Sprintf("%s — waiting for scoped tables in %s to be created", grants, role.Schema)
	}
	qualified := make([]string, len(grantedTables))
	for i, t := range grantedTables {
		qualified[i] = role.Schema + "." + t
	}
	msg := fmt.Sprintf("%s on %s", grants, strings.Join(qualified, ", "))
	if len(grantedTables) < len(role.TableScoping) {
		msg += " (other scoped tables are granted when they're created)"
	}
	return msg
}

func attrString(image map[string]events.DynamoDBAttributeValue, name string) string {
	if v, ok := image[name]; ok && v.DataType() == events.DataTypeString {
		return v.String()
	}
	return ""
}
