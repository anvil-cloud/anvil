# `generate-site-schemas/` — generate component schemas from Go structs

> **Pipeline stage 1** (see [`scripts/README.md`](../README.md)). `main.go`.
>
> | | |
> |---|---|
> | **Runs from** | `provider/` (cwd) |
> | **Reads** | the Go source for each *site* component (`provider/<cloud>/<resource>/*.go`) and the shared inputs in `provider/sites/types.go` |
> | **Writes** | `provider/<cloud>/<resource>/schema.json` for those components |
> | **Invoked by** | `go run ./build gen-site-schemas` (and indirectly by every build, via `merge`) |
> | **Why** | "site" components (e.g. `SvelteKitSite`) share large input structs; hand-writing their schema by hand would be error-prone and duplicative |

## What it does

Most components hand-author their `schema.json` `inputProperties`. **Site
components** don't — their inputs are big and shared across components, so this
script **derives the schema directly from the Go structs** using the Go AST.

It reads struct fields tagged with `pulumi:"..."` (the wire name) and
`schema:"..."` (schema hints), pulls **doc comments** as property descriptions,
resolves **embedded structs** (shared inputs from `provider/sites/`), and emits a
Pulumi `schema.json`.

This is the *opposite direction* from [`generate/`](../generate/README.md):

- `generate-site-schemas` — **Go struct → schema.json** (authoring the component's own inputs)
- `generate` — **schema.json + upstream → schema.json** (adding typed hatch overrides)

Both write to the same per-component `schema.json`; both run before
[`merge/`](../merge/README.md).

## How it works

1. **`components` slice (in `main()`)** — the registry of which components are
   site components and which Go struct is their input type
   (e.g. `SvelteKitSiteInputs`).
2. **AST parse** — `go/parser` reads the struct, walks its fields, and maps each
   tagged field to a schema property. Doc comments become `description`s.
3. **Embedded struct resolution** — shared input structs (from
   `provider/sites/types.go`) are flattened so the schema is complete and
   self-contained.
4. **Enum handling** — the AST can't tell a plain `string` from one that should
   be an enum `$ref`. Two override tables bridge that gap:
   - `enumFieldOverrides` — "this string field should `$ref` a named enum"
   - `manualTypes` — the enum type definitions themselves
5. **Write** — emits `provider/<cloud>/<resource>/schema.json`.

## Adding a new site component

1. Define the input struct in Go with `pulumi:"..."` tags and doc comments
   (reuse shared inputs from `provider/sites/` via embedding where possible).
2. Add a `componentConfig` entry to the `components` slice in `main()`.
3. If any string field should be an enum, add it to `enumFieldOverrides` and
   define the enum in `manualTypes`.
4. Run `go run ./build gen-site-schemas` (or any full build). The `schema.json`
   is regenerated.

## Gotchas

- This **overwrites** the component's `schema.json` from the Go structs, so don't
  hand-edit the generated `inputProperties` for site components — edit the Go
  struct instead.
- The enum override tables are the one manual step; a forgotten entry yields a
  plain `string` instead of a typed enum (no error, just looser typing).
