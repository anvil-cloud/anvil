// Schema generator for Anvil site components.
//
// Reads Go struct definitions with `pulumi:"..."` and `schema:"..."` tags,
// extracts doc comments as descriptions, and emits Pulumi schema.json files.
//
// This generator handles embedded structs (shared inputs from internal/sites/)
// so that schemas are written once and reused across components.
//
// Adding a new site component:
//  1. Add a componentConfig entry to the components slice in main().
//  2. If any string fields should be enums, add entries to enumFieldOverrides
//     and manualTypes below.
//
// Usage:
//
//	cd provider && go run ../scripts/generate-site-schemas/main.go
//
// Or via build.go:
//
//	go run build.go gen-site-schemas
package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ── Enum overrides ────────────────────────────────────────────────────────────
//
// The AST generator cannot distinguish a plain string field from one that
// should reference a named enum type. Add entries here for any string fields
// that should emit a $ref instead of "type": "string".
//
// Key format: "GoStructName.pulumiFieldName"
// Value: the full schema token the field should reference.
//
// The referenced token must also have an entry in manualTypes below.

var enumFieldOverrides = map[string]string{
	"SvelteKitSiteArgs.protection":                    "anvil:aws:SiteProtection",
	"SiteSecurityHeadersArgs.frameOptions":            "anvil:aws:SiteFrameOptions",
	"SiteSecurityHeadersArgs.crossOriginOpenerPolicy": "anvil:aws:SiteCrossOriginOpenerPolicy",
}

// manualTypes holds hand-authored type definitions that are injected into the
// schema types block alongside auto-generated nested object types.
// Use for enum types and any other types the AST generator cannot produce.
var manualTypes = map[string]interface{}{
	"anvil:aws:SiteProtection": map[string]interface{}{
		"type":        "string",
		"description": "Who can invoke the site's server Lambda Function URL. Default: \"none\", or \"edge-oac\" while a WAF is attached or origin protection is enabled. An explicit value always wins.",
		"enum": []map[string]string{
			{
				"name":        "None",
				"value":       "none",
				"description": "Public Function URL. Anyone who learns it can call the server directly, bypassing CloudFront and any WAF or proxy.",
			},
			{
				"name":        "Oac",
				"value":       "oac",
				"description": "Locked to CloudFront via IAM + OAC. Requests with a body are rejected unless they already carry an x-amz-content-sha256 header.",
			},
			{
				"name":        "EdgeOac",
				"value":       "edge-oac",
				"description": "Locked to CloudFront via IAM + OAC, with a Lambda@Edge function that adds the x-amz-content-sha256 header (AWS's documented approach). Nothing is needed in the app; request bodies over 1 MB are rejected.",
			},
		},
	},
	"anvil:aws:SiteFrameOptions": map[string]interface{}{
		"type":        "string",
		"description": "Whether other sites can embed this one in a frame (X-Frame-Options). Default: \"sameorigin\".",
		"enum": []map[string]string{
			{
				"name":        "Sameorigin",
				"value":       "sameorigin",
				"description": "Only pages on this site can frame it. Default.",
			},
			{
				"name":        "Deny",
				"value":       "deny",
				"description": "No site can frame it, including this one.",
			},
			{
				"name":        "None",
				"value":       "none",
				"description": "No X-Frame-Options header — any site can embed it. Use only for sites meant to be embedded elsewhere.",
			},
		},
	},
	"anvil:aws:SiteCrossOriginOpenerPolicy": map[string]interface{}{
		"type":        "string",
		"description": "Cross-Origin-Opener-Policy: isolates the site's browser window from windows on other sites. Default: \"same-origin-allow-popups\".",
		"enum": []map[string]string{
			{
				"name":        "SameOriginAllowPopups",
				"value":       "same-origin-allow-popups",
				"description": "Isolated from other sites, but popups it opens (OAuth sign-in, payments) still work. Default.",
			},
			{
				"name":        "SameOrigin",
				"value":       "same-origin",
				"description": "Strict isolation. Breaks sign-in and payment popups that report back to the page.",
			},
			{
				"name":        "None",
				"value":       "none",
				"description": "No Cross-Origin-Opener-Policy header.",
			},
		},
	},
}

// ── Types ─────────────────────────────────────────────────────────────────────

// schemaProperty represents a single property in a Pulumi schema.
type schemaProperty struct {
	Type                 string          `json:"type,omitempty"`
	Description          string          `json:"description,omitempty"`
	Items                *schemaProperty `json:"items,omitempty"`
	AdditionalProperties *schemaProperty `json:"additionalProperties,omitempty"`
	Ref                  string          `json:"$ref,omitempty"`
}

// componentSchema is the per-resource schema.json fragment.
type componentSchema struct {
	Resources map[string]resourceDef `json:"resources"`
	Types     map[string]interface{} `json:"types"`
}

type resourceDef struct {
	IsComponent     bool                      `json:"isComponent"`
	Description     string                    `json:"description,omitempty"`
	InputProperties map[string]schemaProperty `json:"inputProperties"`
	RequiredInputs  []string                  `json:"requiredInputs,omitempty"`
	Properties      map[string]schemaProperty `json:"properties,omitempty"`
	Required        []string                  `json:"required,omitempty"`
}

// parsedField holds extracted field info from Go AST.
type parsedField struct {
	PulumiName   string
	GoType       string
	GoStructName string // name of the struct this field belongs to, for enum lookup
	Description  string
	Required     bool
	Optional     bool
	IsOutput     bool // true if the Go type is pulumi.*Output
}

// componentConfig describes a component to generate a schema for.
type componentConfig struct {
	// Cloud provider name (aws, gcp).
	Cloud string
	// Resource directory name (sveltekitsite).
	Resource string
	// Pulumi token (e.g. anvil:aws:SvelteKitSite).
	Token string
	// Path to the component's Go file, relative to provider/.
	GoFile string
	// Name of the Args struct in the component file.
	ArgsStruct string
	// Name of the component struct in the component file.
	ComponentStruct string
}

// structInfo holds parsed struct data.
type structInfo struct {
	Doc    string
	Fields []parsedField
	// EmbeddedTypes lists the short names of embedded struct types
	// (e.g. "SvelteKitSiteInputs" from sites.SvelteKitSiteInputs).
	EmbeddedTypes []string
}

// nestedTypeDef is the schema representation of a nested object type.
type nestedTypeDef struct {
	Type        string                    `json:"type"`
	Description string                    `json:"description,omitempty"`
	Properties  map[string]schemaProperty `json:"properties,omitempty"`
	Required    []string                  `json:"required,omitempty"`
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	fmt.Println("🔧 Starting Site Schema Generator...")
	start := time.Now()

	// Define components to generate schemas for.
	// When you add a new site component, add an entry here.
	components := []componentConfig{
		{
			Cloud:           "aws",
			Resource:        "sveltekitsite",
			Token:           "anvil:aws:SvelteKitSite",
			GoFile:          "aws/sveltekitsite/sveltekitsite.go",
			ArgsStruct:      "SvelteKitSiteArgs",
			ComponentStruct: "SvelteKitSite",
		},
		/* {
			Cloud:           "gcp",
			Resource:        "sveltekitsite",
			Token:           "anvil:gcp:SvelteKitSite",
			GoFile:          "gcp/sveltekitsite/sveltekitsite.go",
			ArgsStruct:      "SvelteKitSiteArgs",
			ComponentStruct: "SvelteKitSite",
		}, */
	}

	// Parse the shared types file once — embedded structs reference these.
	sharedTypesFile := "sites/types.go"
	sharedStructs, err := parseGoFile(sharedTypesFile)
	if err != nil {
		log.Fatalf("❌ Failed to parse shared types %s: %v", sharedTypesFile, err)
	}

	for _, comp := range components {
		fmt.Printf("   📦 %s/%s\n", comp.Cloud, comp.Resource)

		// Parse the component's Go file.
		componentStructs, err := parseGoFile(comp.GoFile)
		if err != nil {
			log.Fatalf("❌ Failed to parse %s: %v", comp.GoFile, err)
		}

		// Extract input properties from ArgsStruct.
		inputFields, err := resolveFields(comp.ArgsStruct, componentStructs, sharedStructs)
		if err != nil {
			log.Fatalf("❌ Failed to resolve inputs for %s: %v", comp.Token, err)
		}

		// Extract output properties from ComponentStruct.
		outputFields, err := resolveFields(comp.ComponentStruct, componentStructs, sharedStructs)
		if err != nil {
			log.Fatalf("❌ Failed to resolve outputs for %s: %v", comp.Token, err)
		}

		// Collect nested object types referenced via $ref so we can emit them
		// into the types block. Key = short Go struct name, Value = token name.
		nestedTypes := make(map[string]string) // e.g. "SiteWafArgs" → "anvil:aws:SiteWaf"

		inputProps := make(map[string]schemaProperty)
		var requiredInputs []string
		for _, f := range inputFields {
			prop := goTypeToSchemaProperty(f, comp.Cloud, nestedTypes)
			inputProps[f.PulumiName] = prop
			if f.Required {
				requiredInputs = append(requiredInputs, f.PulumiName)
			}
		}

		outputProps := make(map[string]schemaProperty)
		var requiredOutputs []string
		for _, f := range outputFields {
			if !f.IsOutput {
				continue // Skip non-output fields (ResourceState, etc.)
			}
			outputProps[f.PulumiName] = goTypeToSchemaProperty(f, comp.Cloud, nestedTypes)
			if !f.Optional {
				requiredOutputs = append(requiredOutputs, f.PulumiName)
			}
		}

		// Build the component description from the struct's doc comment.
		compDescription := ""
		if structInfo, ok := componentStructs[comp.ComponentStruct]; ok {
			compDescription = structInfo.Doc
		}

		resDef := resourceDef{
			IsComponent:     true,
			Description:     compDescription,
			InputProperties: inputProps,
			RequiredInputs:  requiredInputs,
			Properties:      outputProps,
			Required:        requiredOutputs,
		}

		// Build types block — emit object definitions for any nested structs
		// referenced via $ref in the input/output properties.
		//
		// BFS loop: processing a nested struct may discover further nested structs.
		// We iterate until no new types are found. A visited set prevents infinite
		// loops on circular references.
		typesBlock := make(map[string]interface{})
		visited := make(map[string]bool) // keyed by goStructName
		pending := nestedTypes           // map[goStructName]tokenName

		for len(pending) > 0 {
			nextPending := make(map[string]string)
			for goStructName, tokenName := range pending {
				if visited[goStructName] {
					continue
				}
				visited[goStructName] = true

				typeDef := buildNestedTypeDef(goStructName, tokenName, componentStructs, sharedStructs, comp.Cloud, nextPending)
				if typeDef != nil {
					typesBlock[tokenName] = typeDef
				}
			}
			// Remove already-visited structs from nextPending before next iteration.
			for goStructName := range visited {
				delete(nextPending, goStructName)
			}
			pending = nextPending
		}

		// Inject manual types (enums and other hand-authored definitions).
		// These are merged in after BFS so they always take precedence over
		// any auto-generated entry with the same token name.
		for token, def := range manualTypes {
			typesBlock[token] = def
		}

		schema := componentSchema{
			Resources: map[string]resourceDef{
				comp.Token: resDef,
			},
			Types: typesBlock,
		}

		// Write schema.json.
		outPath := filepath.Join(comp.Cloud, comp.Resource, "schema.json")
		if err := writeSchemaJSON(outPath, schema); err != nil {
			log.Fatalf("❌ Failed to write %s: %v", outPath, err)
		}
	}

	fmt.Printf("✅ Generated %d site schemas in %v\n", len(components), time.Since(start))
}

// ── Schema building ───────────────────────────────────────────────────────────

// structNameToToken converts a Go struct name to an Anvil schema token.
// Strips the "Args" suffix so SiteWafArgs → anvil:aws:SiteWaf.
func structNameToToken(goName, cloud string) string {
	typeName := strings.TrimSuffix(goName, "Args")
	return fmt.Sprintf("anvil:%s:%s", cloud, typeName)
}

// buildNestedTypeDef constructs the types block entry for a nested struct.
//
// Any pointer-to-struct fields discovered while processing this struct are
// added to nestedTypes so the BFS loop in main can process them next,
// enabling arbitrary nesting depth without recursion limits.
func buildNestedTypeDef(goStructName, tokenName string, componentStructs, sharedStructs map[string]*structInfo, cloud string, nestedTypes map[string]string) *nestedTypeDef {
	// Find the struct — look in component file first, then shared.
	info, ok := componentStructs[goStructName]
	if !ok {
		info, ok = sharedStructs[goStructName]
		if !ok {
			return nil
		}
	}

	props := make(map[string]schemaProperty)
	var required []string

	// Pass nestedTypes through so any pointer-to-struct fields inside this
	// struct are collected for the next BFS iteration.
	for _, f := range info.Fields {
		props[f.PulumiName] = goTypeToSchemaProperty(f, cloud, nestedTypes)
		if !f.Optional {
			required = append(required, f.PulumiName)
		}
	}

	return &nestedTypeDef{
		Type:        "object",
		Description: info.Doc,
		Properties:  props,
		Required:    required,
	}
}

// goTypeToSchemaProperty maps a Go type to a Pulumi schema property.
//
// Resolution order:
//  1. enumFieldOverrides — "StructName.fieldName" → $ref to a named enum token
//  2. Pointer-to-struct  — *SomeArgs → $ref to an auto-generated object token
//  3. Primitive types    — string, int, bool, map, slice
func goTypeToSchemaProperty(f parsedField, cloud string, nestedTypes map[string]string) schemaProperty {
	prop := schemaProperty{
		Description: f.Description,
	}

	// 1. Check enum overrides first.
	// Key is "GoStructName.pulumiFieldName" — matches entries in enumFieldOverrides.
	overrideKey := f.GoStructName + "." + f.PulumiName
	if token, ok := enumFieldOverrides[overrideKey]; ok {
		prop.Ref = fmt.Sprintf("#/types/%s", token)
		return prop
	}

	goType := f.GoType

	// 2. Pointer-to-struct: *SomeArgs → $ref to a named object type.
	if strings.HasPrefix(goType, "*") {
		innerName := strings.TrimPrefix(goType, "*")
		switch innerName {
		case "bool", "int", "int64", "float64", "string":
			// Optional primitives (*bool, *int, …) are just the primitive — the
			// pointer only distinguishes "unset" from the zero value.
			goType = innerName
		default:
			// Shared types from provider/sites (e.g. *sites.SiteWafArgs) resolve
			// against the shared types file, like local structs.
			innerName = strings.TrimPrefix(innerName, "sites.")
			// Only local or provider/sites structs (no other package qualifier).
			if !strings.Contains(innerName, ".") {
				token := structNameToToken(innerName, cloud)
				nestedTypes[innerName] = token
				prop.Ref = fmt.Sprintf("#/types/%s", token)
				return prop
			}
		}
	}

	// 3. Primitive and collection types.
	goType = strings.TrimPrefix(goType, "pulumi.")
	goType = strings.TrimSuffix(goType, "Output")

	switch {
	case goType == "string" || goType == "String":
		prop.Type = "string"
	case goType == "int" || goType == "Int" || goType == "int64":
		prop.Type = "integer"
	case goType == "float64" || goType == "Float64":
		prop.Type = "number"
	case goType == "bool" || goType == "Bool":
		prop.Type = "boolean"
	case strings.HasPrefix(goType, "map[string]string") || strings.HasPrefix(goType, "Map"):
		prop.Type = "object"
		prop.AdditionalProperties = &schemaProperty{Type: "string"}
	case goType == "map[string]pulumi.StringInput":
		// Values may be plain strings or Pulumi outputs (e.g. runtimeEnvironment).
		prop.Type = "object"
		prop.AdditionalProperties = &schemaProperty{Type: "string"}
	case strings.HasPrefix(goType, "[]"):
		prop.Type = "array"
		elemType := strings.TrimPrefix(goType, "[]")
		prop.Items = &schemaProperty{Type: goTypeToSchemaType(elemType)}
	default:
		prop.Type = "string"
	}

	return prop
}

// goTypeToSchemaType maps a simple Go type name to a Pulumi schema type string.
func goTypeToSchemaType(goType string) string {
	switch goType {
	case "string":
		return "string"
	case "int", "int64":
		return "integer"
	case "float64":
		return "number"
	case "bool":
		return "boolean"
	default:
		return "string"
	}
}

// ── Go AST parsing ────────────────────────────────────────────────────────────

// parseGoFile parses a Go source file and extracts all struct definitions
// with their fields, tags, and doc comments.
func parseGoFile(path string) (map[string]*structInfo, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}

	structs := make(map[string]*structInfo)

	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}

		for _, spec := range genDecl.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}

			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}

			info := &structInfo{}
			structName := typeSpec.Name.Name

			// Extract doc comment from the type declaration.
			if genDecl.Doc != nil {
				info.Doc = cleanDocComment(genDecl.Doc.Text())
			}
			// TypeSpec-level doc takes precedence if present.
			if typeSpec.Doc != nil {
				info.Doc = cleanDocComment(typeSpec.Doc.Text())
			}

			for _, field := range structType.Fields.List {
				// Handle embedded struct (no names).
				if len(field.Names) == 0 {
					embeddedName := resolveTypeName(field.Type)
					if embeddedName != "" {
						info.EmbeddedTypes = append(info.EmbeddedTypes, embeddedName)
					}
					continue
				}

				// Named field — extract pulumi tag and doc comment.
				if field.Tag == nil {
					continue
				}

				tag := field.Tag.Value
				pulumiTag := extractTag(tag, "pulumi")
				if pulumiTag == "" {
					continue
				}

				pf := parsedField{
					GoStructName: structName,
				}

				// Parse pulumi tag: "name,optional" or just "name".
				parts := strings.SplitN(pulumiTag, ",", 2)
				pf.PulumiName = parts[0]
				if len(parts) > 1 && parts[1] == "optional" {
					pf.Optional = true
				}

				// Parse schema tag for "required" override.
				schemaTag := extractTag(tag, "schema")
				if strings.Contains(schemaTag, "required") {
					pf.Required = true
					pf.Optional = false
				} else if !pf.Optional {
					// Default to optional — safe assumption.
					pf.Optional = true
				}

				// Extract Go type as string.
				pf.GoType = typeToString(field.Type)

				// Check if it's a pulumi.*Output type.
				pf.IsOutput = strings.Contains(pf.GoType, "Output")

				// Extract doc comment.
				if field.Doc != nil {
					pf.Description = cleanDocComment(field.Doc.Text())
				}

				info.Fields = append(info.Fields, pf)
			}

			structs[structName] = info
		}
	}

	return structs, nil
}

// resolveFields collects all fields for a struct, including fields from
// embedded structs. It looks up embedded types in both the component file's
// structs and the shared types file's structs.
func resolveFields(structName string, componentStructs, sharedStructs map[string]*structInfo) ([]parsedField, error) {
	info, ok := componentStructs[structName]
	if !ok {
		return nil, fmt.Errorf("struct %s not found in component file", structName)
	}

	var allFields []parsedField

	// First, resolve embedded types.
	for _, embeddedName := range info.EmbeddedTypes {
		if shared, ok := sharedStructs[embeddedName]; ok {
			embedded, err := resolveFieldsFromInfo(shared, sharedStructs)
			if err != nil {
				return nil, fmt.Errorf("resolving embedded %s: %w", embeddedName, err)
			}
			allFields = append(allFields, embedded...)
		} else if local, ok := componentStructs[embeddedName]; ok {
			embedded, err := resolveFieldsFromInfo(local, componentStructs)
			if err != nil {
				return nil, fmt.Errorf("resolving embedded %s: %w", embeddedName, err)
			}
			allFields = append(allFields, embedded...)
		}
		// Skip unresolvable embeds (like pulumi.ResourceState) — not schema fields.
	}

	// Then add the struct's own fields.
	allFields = append(allFields, info.Fields...)

	return allFields, nil
}

// resolveFieldsFromInfo is the recursive helper for resolveFields.
func resolveFieldsFromInfo(info *structInfo, available map[string]*structInfo) ([]parsedField, error) {
	var allFields []parsedField

	for _, embeddedName := range info.EmbeddedTypes {
		if embedded, ok := available[embeddedName]; ok {
			fields, err := resolveFieldsFromInfo(embedded, available)
			if err != nil {
				return nil, err
			}
			allFields = append(allFields, fields...)
		}
	}

	allFields = append(allFields, info.Fields...)
	return allFields, nil
}

// ── AST helpers ───────────────────────────────────────────────────────────────

// typeToString converts an ast.Expr type to a readable string.
func typeToString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeToString(t.X) + "." + t.Sel.Name
	case *ast.MapType:
		return "map[" + typeToString(t.Key) + "]" + typeToString(t.Value)
	case *ast.ArrayType:
		return "[]" + typeToString(t.Elt)
	case *ast.StarExpr:
		return "*" + typeToString(t.X)
	case *ast.InterfaceType:
		return "interface{}"
	default:
		return "unknown"
	}
}

// resolveTypeName extracts the short struct name from an embedded type expression.
// e.g. sites.SvelteKitSiteInputs → SvelteKitSiteInputs
//
//	pulumi.ResourceState → ResourceState
func resolveTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return t.Sel.Name
	case *ast.StarExpr:
		return resolveTypeName(t.X)
	default:
		return ""
	}
}

// extractTag pulls a specific tag value from a raw Go struct tag string.
// e.g. extractTag(`pulumi:"path" schema:"required"`, "pulumi") → "path"
func extractTag(raw string, key string) string {
	raw = strings.Trim(raw, "`")

	search := key + `:"`
	idx := strings.Index(raw, search)
	if idx == -1 {
		return ""
	}

	start := idx + len(search)
	end := strings.Index(raw[start:], `"`)
	if end == -1 {
		return ""
	}

	return raw[start : start+end]
}

// cleanDocComment trims and normalises a Go doc comment string.
func cleanDocComment(s string) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	var cleaned []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			cleaned = append(cleaned, line)
		}
	}
	return strings.Join(cleaned, " ")
}

// ── Output ────────────────────────────────────────────────────────────────────

// writeSchemaJSON writes a componentSchema to disk as indented JSON.
func writeSchemaJSON(path string, schema componentSchema) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}
