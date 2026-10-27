package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/jackc/pgx/v5"
)

// ── Table changes against the real catalog ──────────────────────────────────
//
// An existing table is compared with what the database actually has, not with
// the item's previous definition. So a column whose add failed is retried on
// the next change, and a column that exists with another type is reported
// instead of silently skipped. Changes stay additive: missing columns and
// indexes are added; nothing is dropped or altered.
//
// DSQL's ALTER TABLE ADD COLUMN takes only a name and a type, so a column
// added to an existing table can't have NOT NULL or DEFAULT. Those columns are
// rejected (state failed) rather than worked around. A new table has no such
// limit: CREATE TABLE supports both.

// addColumnLimit explains the DSQL limit in a rejected column's status.
const addColumnLimit = "DSQL doesn't support NOT NULL or DEFAULT when adding a column to an existing table " +
	"(see ALTER TABLE in the Aurora DSQL docs). Add it as a nullable column with no default, " +
	"or declare it when the table is first created"

type dbColumn struct {
	Type    string // format_type() output, e.g. "character varying(20)"
	NotNull bool
}

type tablePlan struct {
	AddColumns []columnDef
	AddIndexes []indexDef
	Rejected   []string
	Drift      []string
}

// expectedType is the catalog's name for a declared column type, as
// format_type() reports it in DSQL.
func expectedType(c columnDef) string {
	switch c.Type {
	case "varchar":
		if c.Length > 0 {
			return fmt.Sprintf("character varying(%d)", c.Length)
		}
		return "character varying"
	case "char":
		if c.Length > 0 {
			return fmt.Sprintf("character(%d)", c.Length)
		}
		return "character(1)"
	case "numeric":
		if c.Precision > 0 {
			return fmt.Sprintf("numeric(%d,%d)", c.Precision, c.Scale)
		}
		return "numeric(18,6)" // DSQL's default precision and scale
	case "time":
		return "time without time zone"
	case "timestamp":
		return "timestamp without time zone"
	case "timestamptz":
		return "timestamp with time zone"
	default:
		return c.Type
	}
}

// tableIndexes is every index the definition implies: declared indexes plus
// one unique index per unique column.
func tableIndexes(def tableDef) []indexDef {
	idx := append([]indexDef{}, def.Indexes...)
	for _, c := range def.Columns {
		if c.Unique {
			idx = append(idx, indexDef{Name: fmt.Sprintf("%s_%s_key", def.Name, c.Name), Columns: []string{c.Name}, Unique: true})
		}
	}
	return idx
}

// planTableChanges compares a definition with an existing table's columns and
// index names. Columns the database has but the definition doesn't are left
// alone (removals are human-owned).
func planTableChanges(def tableDef, cols map[string]dbColumn, indexes map[string]bool) tablePlan {
	var plan tablePlan
	pk := map[string]bool{}
	for _, c := range def.PrimaryKey {
		pk[c] = true
	}
	rejected := map[string]bool{}

	for _, c := range def.Columns {
		db, ok := cols[c.Name]
		if !ok {
			if c.NotNull || c.Default != "" {
				plan.Rejected = append(plan.Rejected, fmt.Sprintf("column %q: %s", c.Name, addColumnLimit))
				rejected[c.Name] = true
				continue
			}
			plan.AddColumns = append(plan.AddColumns, c)
			continue
		}
		if want := expectedType(c); db.Type != want {
			plan.Drift = append(plan.Drift, fmt.Sprintf("column %q is %s in your code but %s in the database", c.Name, want, db.Type))
		}
		if wantNotNull := c.NotNull || pk[c.Name]; wantNotNull != db.NotNull {
			if wantNotNull {
				plan.Drift = append(plan.Drift, fmt.Sprintf("column %q is NOT NULL in your code but allows NULL in the database", c.Name))
			} else {
				plan.Drift = append(plan.Drift, fmt.Sprintf("column %q allows NULL in your code but is NOT NULL in the database", c.Name))
			}
		}
	}

	for _, idx := range tableIndexes(def) {
		if indexes[idx.Name] {
			continue
		}
		skip := false
		for _, c := range idx.Columns {
			skip = skip || rejected[c]
		}
		if !skip {
			plan.AddIndexes = append(plan.AddIndexes, idx)
		}
	}
	return plan
}

// planOutcome turns a plan (after it was applied) into the table's status.
func planOutcome(plan tablePlan) outcome {
	var done []string
	for _, c := range plan.AddColumns {
		done = append(done, fmt.Sprintf("added column %s", c.Name))
	}
	for _, idx := range plan.AddIndexes {
		done = append(done, fmt.Sprintf("building index %s", idx.Name))
	}
	notes := append(append([]string{}, plan.Rejected...), plan.Drift...)
	switch {
	case len(plan.Rejected) > 0:
		return outcome{State: stateFailed, Message: strings.Join(append(notes, done...), "; ")}
	case len(plan.Drift) > 0:
		return outcome{State: stateDrift, Message: strings.Join(append(notes, done...), "; ")}
	case len(done) > 0:
		return outcome{State: stateApplied, Message: strings.Join(done, "; ")}
	default:
		return outcome{State: stateApplied, Message: "up to date"}
	}
}

func tableExists(ctx context.Context, conn *pgx.Conn, schema, table string) (bool, error) {
	var exists bool
	err := conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = $1 AND tablename = $2)", schema, table,
	).Scan(&exists)
	return exists, err
}

func tableColumns(ctx context.Context, conn *pgx.Conn, schema, table string) (map[string]dbColumn, error) {
	rows, err := conn.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped`, schema, table)
	if err != nil {
		return nil, fmt.Errorf("reading columns of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	cols := map[string]dbColumn{}
	for rows.Next() {
		var name string
		var col dbColumn
		if err := rows.Scan(&name, &col.Type, &col.NotNull); err != nil {
			return nil, err
		}
		cols[name] = col
	}
	return cols, rows.Err()
}

func tableIndexNames(ctx context.Context, conn *pgx.Conn, schema, table string) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2", schema, table)
	if err != nil {
		return nil, fmt.Errorf("reading indexes of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()
	names := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = true
	}
	return names, rows.Err()
}

// reconcileTable creates the table if it's missing, otherwise applies the
// additive changes the catalog says are needed. newItem is true for an
// INSERT record: that's when roles scoped to the table may be waiting for it.
func reconcileTable(ctx context.Context, ddb rolesTableAPI, def tableDef, newItem bool) (outcome, error) {
	conn, err := connectAdmin(ctx)
	if err != nil {
		return outcome{}, err
	}
	defer conn.Close(ctx)

	exists, err := tableExists(ctx, conn, def.Schema, def.Name)
	if err != nil {
		return outcome{}, err
	}
	if !exists {
		if err := createTable(ctx, conn, def); err != nil {
			return outcome{}, err
		}
		if err := grantScopedRolesOnTable(ctx, conn, ddb, def); err != nil {
			return outcome{}, err
		}
		return outcome{State: stateApplied, Message: fmt.Sprintf("created with %d columns", len(def.Columns))}, nil
	}

	cols, err := tableColumns(ctx, conn, def.Schema, def.Name)
	if err != nil {
		return outcome{}, err
	}
	indexes, err := tableIndexNames(ctx, conn, def.Schema, def.Name)
	if err != nil {
		return outcome{}, err
	}
	plan := planTableChanges(def, cols, indexes)
	log.Printf("[dsql-bootstrap] table %s.%s exists — %d column(s) and %d index(es) to add, %d rejected, %d drift",
		def.Schema, def.Name, len(plan.AddColumns), len(plan.AddIndexes), len(plan.Rejected), len(plan.Drift))

	for _, c := range plan.AddColumns {
		sql := fmt.Sprintf("ALTER TABLE %s.%s ADD COLUMN IF NOT EXISTS %s",
			pgIdent(def.Schema), pgIdent(def.Name), buildColumnDDL(c))
		if err := execAll(ctx, conn, []string{sql}); err != nil {
			return outcome{}, err
		}
	}
	for _, idx := range plan.AddIndexes {
		if err := createIndexAsync(ctx, conn, def, idx); err != nil {
			return outcome{}, err
		}
	}
	if newItem {
		if err := grantScopedRolesOnTable(ctx, conn, ddb, def); err != nil {
			return outcome{}, err
		}
	}
	return planOutcome(plan), nil
}
