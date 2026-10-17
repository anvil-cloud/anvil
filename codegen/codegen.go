package codegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// ParseSchemaFile reads and validates an anvil.schema.json file.
func ParseSchemaFile(path string) (*SchemaFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading schema file: %w", err)
	}

	var sf SchemaFile
	if err := json.Unmarshal(data, &sf); err != nil {
		return nil, fmt.Errorf("parsing schema file: %w", err)
	}

	if err := validate(&sf); err != nil {
		return nil, err
	}

	normalize(&sf)
	return &sf, nil
}

// Run parses the schema file and runs the given generator.
func Run(schemaPath string, gen Generator, outDir string) error {
	sf, err := ParseSchemaFile(schemaPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	return gen.Generate(sf.DSQL, outDir)
}

// RunAll parses the schema file and runs all given generators.
func RunAll(schemaPath string, generators []Generator, outDir string) error {
	sf, err := ParseSchemaFile(schemaPath)
	if err != nil {
		return err
	}

	for _, gen := range generators {
		dir := filepath.Join(outDir, gen.Name())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating output directory for %s: %w", gen.Name(), err)
		}
		if err := gen.Generate(sf.DSQL, dir); err != nil {
			return fmt.Errorf("%s generator: %w", gen.Name(), err)
		}
	}
	return nil
}

// RunFromManifest runs a generator using schema data extracted from the build manifest.
// Each entry maps a component name to raw JSON schemas (matching DSQLComponent.Schemas).
func RunFromManifest(entries []ManifestSchemaEntry, gen Generator, outDir string) error {
	schemas := make(map[string]DSQLComponent)
	for _, e := range entries {
		var comp DSQLComponent
		if err := json.Unmarshal(e.SchemasJSON, &comp.Schemas); err != nil {
			return fmt.Errorf("parsing schemas for component %q: %w", e.Component, err)
		}
		schemas[e.Component] = comp
	}

	sf := &SchemaFile{DSQL: schemas}
	if err := validate(sf); err != nil {
		return err
	}
	normalize(sf)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	return gen.Generate(sf.DSQL, outDir)
}

// ManifestSchemaEntry represents a single DSQL component's schemas from the build manifest.
type ManifestSchemaEntry struct {
	Component   string
	SchemasJSON json.RawMessage
}

// validate mirrors the validation logic from provider/aws/dsql/dsql.go:validateSchemas.
func validate(sf *SchemaFile) error {
	for componentName, comp := range sf.DSQL {
		roleSchema := map[string]string{}
		distinct := map[string]bool{}

		for si := range comp.Schemas {
			s := &comp.Schemas[si]
			if s.Name == "" {
				return fmt.Errorf("component %q: schema name must not be empty", componentName)
			}
			if s.Name == "public" {
				return fmt.Errorf("component %q schema %q: cannot manage the public schema", componentName, s.Name)
			}
			distinct[s.Name] = true

			for _, r := range s.Roles {
				if r.Name == "" {
					return fmt.Errorf("component %q schema %q: role name must not be empty", componentName, s.Name)
				}
				if len(r.Grants) == 0 {
					return fmt.Errorf("component %q schema %q role %q: grants must not be empty", componentName, s.Name, r.Name)
				}
				if existing, dup := roleSchema[r.Name]; dup {
					return fmt.Errorf("component %q: role %q declared in both schema %q and %q — names must be globally unique", componentName, r.Name, existing, s.Name)
				}
				roleSchema[r.Name] = s.Name
			}

			tableNames := map[string]bool{}
			for _, t := range s.Tables {
				if t.Name == "" {
					return fmt.Errorf("component %q schema %q: table name must not be empty", componentName, s.Name)
				}
				tableNames[t.Name] = true
				if len(t.Columns) == 0 {
					return fmt.Errorf("component %q table %s.%s: must have at least one column", componentName, s.Name, t.Name)
				}
				if len(t.PrimaryKey) == 0 {
					return fmt.Errorf("component %q table %s.%s: primaryKey is required", componentName, s.Name, t.Name)
				}

				colByName := map[string]bool{}
				for _, c := range t.Columns {
					if c.Name == "" {
						return fmt.Errorf("component %q table %s.%s: column name must not be empty", componentName, s.Name, t.Name)
					}
					if !supportedTypes[c.Type] {
						return fmt.Errorf("component %q table %s.%s column %q: unsupported type %q", componentName, s.Name, t.Name, c.Name, c.Type)
					}
					switch c.Type {
					case "varchar":
						if c.Length > 65535 {
							return fmt.Errorf("component %q table %s.%s column %q: varchar length %d exceeds max 65535", componentName, s.Name, t.Name, c.Name, c.Length)
						}
					case "char":
						if c.Length > 4096 {
							return fmt.Errorf("component %q table %s.%s column %q: char length %d exceeds max 4096", componentName, s.Name, t.Name, c.Name, c.Length)
						}
					case "numeric":
						if c.Precision > 38 {
							return fmt.Errorf("component %q table %s.%s column %q: numeric precision %d exceeds max 38", componentName, s.Name, t.Name, c.Name, c.Precision)
						}
						if c.Scale > 37 {
							return fmt.Errorf("component %q table %s.%s column %q: numeric scale %d exceeds max 37", componentName, s.Name, t.Name, c.Name, c.Scale)
						}
					}
					colByName[c.Name] = true
				}

				for _, pk := range t.PrimaryKey {
					if !colByName[pk] {
						return fmt.Errorf("component %q table %s.%s: primaryKey column %q not found in columns", componentName, s.Name, t.Name, pk)
					}
				}

				for _, idx := range t.Indexes {
					if idx.Name == "" {
						return fmt.Errorf("component %q table %s.%s: every index requires a name", componentName, s.Name, t.Name)
					}
					if len(idx.Columns) == 0 {
						return fmt.Errorf("component %q table %s.%s index %q: must reference at least one column", componentName, s.Name, t.Name, idx.Name)
					}
					for _, ic := range idx.Columns {
						if !colByName[ic] {
							return fmt.Errorf("component %q table %s.%s index %q: column %q not found", componentName, s.Name, t.Name, idx.Name, ic)
						}
					}
				}
			}

			for _, r := range s.Roles {
				for _, scoped := range r.TableScoping {
					if !tableNames[scoped] {
						return fmt.Errorf("component %q schema %q role %q: tableScoping references undeclared table %q", componentName, s.Name, r.Name, scoped)
					}
				}
			}
		}

		if len(distinct) > 10 {
			return fmt.Errorf("component %q: DSQL allows at most 10 schemas; %d declared", componentName, len(distinct))
		}
	}
	return nil
}

// normalize applies auto-NOT-NULL to primary key columns.
func normalize(sf *SchemaFile) {
	for _, comp := range sf.DSQL {
		for si := range comp.Schemas {
			s := &comp.Schemas[si]
			for ti := range s.Tables {
				t := &s.Tables[ti]
				pkSet := map[string]bool{}
				for _, pk := range t.PrimaryKey {
					pkSet[pk] = true
				}
				for ci := range t.Columns {
					if pkSet[t.Columns[ci].Name] {
						t.Columns[ci].NotNull = true
					}
				}
			}
		}
	}
}

// Helpers for name conversion used by generators.

func ToPascalCase(s string) string {
	parts := splitIdentifier(s)
	var b strings.Builder
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		runes := []rune(p)
		runes[0] = unicode.ToUpper(runes[0])
		b.WriteString(string(runes))
	}
	return b.String()
}

func ToCamelCase(s string) string {
	pc := ToPascalCase(s)
	if len(pc) == 0 {
		return pc
	}
	runes := []rune(pc)
	runes[0] = unicode.ToLower(runes[0])
	return string(runes)
}

func ToSnakeCase(s string) string {
	parts := splitIdentifier(s)
	for i := range parts {
		parts[i] = strings.ToLower(parts[i])
	}
	return strings.Join(parts, "_")
}

func splitIdentifier(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == ' '
	})
}

// insertRequired reports whether a table's Insert type must carry the column:
// it has no default and is a primary key or NOT NULL. A column with a default
// is optional (a primary key included, e.g. `default gen_random_uuid()`), and
// each generated insert leaves it out of the INSERT when unset so the
// database applies the default instead of receiving an explicit NULL.
func insertRequired(t Table, c Column) bool {
	if c.Default != "" {
		return false
	}
	if c.NotNull {
		return true
	}
	for _, pk := range t.PrimaryKey {
		if pk == c.Name {
			return true
		}
	}
	return false
}
