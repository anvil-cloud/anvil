package codegen

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSchemaFile(t *testing.T) {
	sf, err := ParseSchemaFile(filepath.Join("testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatalf("ParseSchemaFile: %v", err)
	}

	comp, ok := sf.DSQL["orders"]
	if !ok {
		t.Fatal("expected 'orders' component")
	}
	if len(comp.Schemas) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(comp.Schemas))
	}

	s := comp.Schemas[0]
	if s.Name != "myapp" {
		t.Errorf("expected schema name 'myapp', got %q", s.Name)
	}
	if len(s.Tables) != 3 {
		t.Fatalf("expected 3 tables, got %d", len(s.Tables))
	}

	users := s.Tables[0]
	if users.Name != "users" {
		t.Errorf("expected first table 'users', got %q", users.Name)
	}
	if !users.Columns[0].NotNull {
		t.Error("expected PK column 'id' to be auto-NOT-NULL")
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		sf      SchemaFile
		wantErr string
	}{
		{
			name: "empty schema name",
			sf: SchemaFile{DSQL: map[string]DSQLComponent{
				"test": {Schemas: []Schema{{Name: ""}}},
			}},
			wantErr: "schema name must not be empty",
		},
		{
			name: "public schema",
			sf: SchemaFile{DSQL: map[string]DSQLComponent{
				"test": {Schemas: []Schema{{Name: "public"}}},
			}},
			wantErr: "cannot manage the public schema",
		},
		{
			name: "unsupported type",
			sf: SchemaFile{DSQL: map[string]DSQLComponent{
				"test": {Schemas: []Schema{{
					Name: "app",
					Tables: []Table{{
						Name:       "t1",
						PrimaryKey: []string{"id"},
						Columns: []Column{
							{Name: "id", Type: "serial"},
						},
					}},
				}}},
			}},
			wantErr: "unsupported type",
		},
		{
			name: "missing PK column",
			sf: SchemaFile{DSQL: map[string]DSQLComponent{
				"test": {Schemas: []Schema{{
					Name: "app",
					Tables: []Table{{
						Name:       "t1",
						PrimaryKey: []string{"nope"},
						Columns: []Column{
							{Name: "id", Type: "uuid"},
						},
					}},
				}}},
			}},
			wantErr: "primaryKey column \"nope\" not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(&tt.sf)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestTypeScriptGeneration(t *testing.T) {
	sf, err := ParseSchemaFile(filepath.Join("testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatalf("ParseSchemaFile: %v", err)
	}

	outDir := t.TempDir()
	gen := &TypeScriptGenerator{}
	if err := gen.Generate(sf.DSQL, outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "export interface UsersRow")
	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "export interface UsersInsert")
	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "export interface OrdersRow")
	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "createMyappQueries")
	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "findById")
	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "insert")
	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "update")
	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "delete")
	assertFileContains(t, filepath.Join(outDir, "index.ts"), "export * from './myapp'")
}

func TestPythonGeneration(t *testing.T) {
	sf, err := ParseSchemaFile(filepath.Join("testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatalf("ParseSchemaFile: %v", err)
	}

	outDir := t.TempDir()
	gen := &PythonGenerator{}
	if err := gen.Generate(sf.DSQL, outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	assertFileContains(t, filepath.Join(outDir, "myapp.py"), "class UsersRow")
	assertFileContains(t, filepath.Join(outDir, "myapp.py"), "class UsersInsert")
	assertFileContains(t, filepath.Join(outDir, "myapp.py"), "class MyappQueries")
	assertFileContains(t, filepath.Join(outDir, "myapp.py"), "find_by_id")
	assertFileContains(t, filepath.Join(outDir, "__init__.py"), "from .myapp import")
}

func TestGoGeneration(t *testing.T) {
	sf, err := ParseSchemaFile(filepath.Join("testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatalf("ParseSchemaFile: %v", err)
	}

	outDir := t.TempDir()
	gen := &GoGenerator{}
	if err := gen.Generate(sf.DSQL, outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	assertFileContains(t, filepath.Join(outDir, "myapp.go"), "type UsersRow struct")
	assertFileContains(t, filepath.Join(outDir, "myapp.go"), "type UsersInsert struct")
	assertFileContains(t, filepath.Join(outDir, "myapp.go"), "func (q *UsersQueries) FindByID")
	assertFileContains(t, filepath.Join(outDir, "myapp.go"), "type OrdersRow struct")
}

func TestRustGeneration(t *testing.T) {
	sf, err := ParseSchemaFile(filepath.Join("testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatalf("ParseSchemaFile: %v", err)
	}

	outDir := t.TempDir()
	gen := &RustGenerator{}
	if err := gen.Generate(sf.DSQL, outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	module := filepath.Join(outDir, "src", "myapp.rs")
	assertFileContains(t, module, "pub struct UsersRow")
	assertFileContains(t, module, "pub struct UsersInsert")
	assertFileContains(t, module, "impl<'a> UsersQueries<'a>")
	assertFileContains(t, module, "pub struct OrdersRow")
	assertFileContains(t, filepath.Join(outDir, "src", "lib.rs"), "pub mod myapp;")

	manifest := filepath.Join(outDir, "Cargo.toml")
	assertFileContains(t, manifest, `name = "db"`)
	assertFileContains(t, manifest, "tokio-postgres = { version = \"0.7\"")
	assertFileContains(t, manifest, `serde = { version = "1", features = ["derive"] }`)

	// The dependency list moved into Cargo.toml; no copy-me comment remains.
	data, err := os.ReadFile(module)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Add these to your Cargo.toml") {
		t.Error("module still carries the old Cargo.toml instructions")
	}
}

func TestRustGenerationIsDeterministicAndPrunes(t *testing.T) {
	sf, err := ParseSchemaFile(filepath.Join("testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatalf("ParseSchemaFile: %v", err)
	}

	outDir := t.TempDir()
	srcDir := filepath.Join(outDir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(srcDir, "dropped.rs")
	handWritten := filepath.Join(srcDir, "helpers.rs")
	if err := os.WriteFile(stale, []byte(rustGeneratedHeader+"\npub struct Old;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handWritten, []byte("// mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	gen := &RustGenerator{}
	snapshot := func() string {
		if err := gen.Generate(sf.DSQL, outDir); err != nil {
			t.Fatalf("Generate: %v", err)
		}
		var b strings.Builder
		for _, f := range []string{"Cargo.toml", "src/lib.rs", "src/myapp.rs"} {
			data, err := os.ReadFile(filepath.Join(outDir, f))
			if err != nil {
				t.Fatal(err)
			}
			b.Write(data)
		}
		return b.String()
	}

	first := snapshot()
	for i := 0; i < 5; i++ {
		if snapshot() != first {
			t.Fatal("generated crate differs between runs")
		}
	}

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("module for a removed schema was not deleted")
	}
	if _, err := os.Stat(handWritten); err != nil {
		t.Error("hand-written file in src/ was deleted")
	}
}

func TestRunFromManifest(t *testing.T) {
	sf, err := ParseSchemaFile(filepath.Join("testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatalf("ParseSchemaFile: %v", err)
	}

	var entries []ManifestSchemaEntry
	for name, comp := range sf.DSQL {
		raw, err := json.Marshal(comp.Schemas)
		if err != nil {
			t.Fatalf("marshaling schemas: %v", err)
		}
		entries = append(entries, ManifestSchemaEntry{
			Component:   name,
			SchemasJSON: raw,
		})
	}

	outDir := t.TempDir()
	gen := &TypeScriptGenerator{}
	if err := RunFromManifest(entries, gen, outDir); err != nil {
		t.Fatalf("RunFromManifest: %v", err)
	}

	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "export interface UsersRow")
	assertFileContains(t, filepath.Join(outDir, "myapp.ts"), "createMyappQueries")
	assertFileContains(t, filepath.Join(outDir, "index.ts"), "export * from './myapp'")
}

func TestNameConversions(t *testing.T) {
	tests := []struct {
		input  string
		pascal string
		camel  string
		snake  string
	}{
		{"user_id", "UserId", "userId", "user_id"},
		{"created_at", "CreatedAt", "createdAt", "created_at"},
		{"myapp", "Myapp", "myapp", "myapp"},
		{"display-name", "DisplayName", "displayName", "display_name"},
	}

	for _, tt := range tests {
		if got := ToPascalCase(tt.input); got != tt.pascal {
			t.Errorf("ToPascalCase(%q) = %q, want %q", tt.input, got, tt.pascal)
		}
		if got := ToCamelCase(tt.input); got != tt.camel {
			t.Errorf("ToCamelCase(%q) = %q, want %q", tt.input, got, tt.camel)
		}
		if got := ToSnakeCase(tt.input); got != tt.snake {
			t.Errorf("ToSnakeCase(%q) = %q, want %q", tt.input, got, tt.snake)
		}
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("%s does not contain %q\n\nFull content:\n%s", path, want, string(data))
	}
}

// A column with a default is optional in the Insert type (a primary key
// included) and, when unset, is left out of the INSERT so the database fills
// it in. Binding it as NULL instead violates NOT NULL (orders.status).
func TestInsertOmitsUnsetDefaults(t *testing.T) {
	sf, err := ParseSchemaFile(filepath.Join("testdata", "anvil.schema.json"))
	if err != nil {
		t.Fatalf("ParseSchemaFile: %v", err)
	}
	generate := func(g Generator) string {
		t.Helper()
		outDir := t.TempDir()
		if err := g.Generate(sf.DSQL, outDir); err != nil {
			t.Fatalf("%s Generate: %v", g.Name(), err)
		}
		return outDir
	}

	t.Run("rust", func(t *testing.T) {
		module := filepath.Join(generate(&RustGenerator{}), "src", "myapp.rs")
		// Required columns are plain types and always bound.
		assertFileContains(t, module, "pub struct OrdersInsert {\n    pub id: uuid::Uuid,\n    pub user_id: uuid::Uuid,")
		assertFileContains(t, module, "        cols.push(\"user_id\");\n        params.push(&row.user_id);\n")
		// Defaulted columns are bound only when Some.
		assertFileContains(t, module, "    pub status: Option<String>,")
		assertFileContains(t, module, "        if let Some(v) = &row.status {\n            cols.push(\"status\");\n            params.push(v);\n        }\n")
		assertFileContains(t, module, "RETURNING id, user_id, total_amount, status, notes, placed_at\", cols.join(\", \"), placeholders.join(\", \"));")
		// A defaulted primary key is optional too.
		assertFileContains(t, module, "pub struct EventsInsert {\n    pub id: Option<uuid::Uuid>,")
		// With every column defaulted, an empty insert uses DEFAULT VALUES.
		assertFileContains(t, module, "\"INSERT INTO myapp.events DEFAULT VALUES RETURNING id, created_at\"")
		// No defaulted columns: the fixed statement stays.
		assertFileContains(t, module, "\"INSERT INTO myapp.users ")
		data, _ := os.ReadFile(module)
		if strings.Contains(string(data), "INSERT INTO myapp.orders (id, user_id, total_amount, status, notes, placed_at) VALUES") {
			t.Error("orders insert still binds every column")
		}
	})

	t.Run("go", func(t *testing.T) {
		file := filepath.Join(generate(&GoGenerator{}), "myapp.go")
		assertFileContains(t, file, "\tif row.Status != nil {\n\t\tcols = append(cols, \"status\")\n\t\targs = append(args, row.Status)\n\t}\n")
		assertFileContains(t, file, "\tcols = append(cols, \"user_id\")\n\targs = append(args, row.UserId)\n")
		assertFileContains(t, file, "type EventsInsert struct {\n\tId *string")
		assertFileContains(t, file, "query = \"INSERT INTO myapp.events DEFAULT VALUES RETURNING id, created_at\"")
		assertFileContains(t, file, "\t\"strconv\"\n\t\"strings\"\n")
		// RETURNING scans into the result, not the caller's Insert value.
		assertFileContains(t, file, "QueryRowContext(ctx, query, args...).Scan(&result.Id, &result.UserId,")
	})

	t.Run("typescript", func(t *testing.T) {
		file := filepath.Join(generate(&TypeScriptGenerator{}), "myapp.ts")
		assertFileContains(t, file, "export interface EventsInsert {\n  id?: string;")
		assertFileContains(t, file, "`INSERT INTO myapp.events DEFAULT VALUES RETURNING *`")
	})

	t.Run("python", func(t *testing.T) {
		file := filepath.Join(generate(&PythonGenerator{}), "myapp.py")
		assertFileContains(t, file, "class EventsInsert:\n    id: Optional[str] = None")
		assertFileContains(t, file, "sql = 'INSERT INTO myapp.events DEFAULT VALUES RETURNING *'")
	})
}
