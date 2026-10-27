# DSQL schema, role and grant management — test report

**Date:** 27 October 2026
**Branch:** `dsqlVPC` (changes uncommitted at the time of testing)
**Region:** `ap-southeast-2`
**Apps used:** `~/Desktop/testForwardRef`, and five apps in `~/Desktop/dsql-rerun/`

Everything in this report was run against real AWS (DSQL, Lambda, DynamoDB),
not mocked. Unit tests are listed separately at the end.

---

## 1. Scope

What was under test is how Anvil applies a DSQL component's `schemas` (tables,
roles, grants) and `grantConnect` mappings: the roles table, its stream, the
bootstrap Lambda, and — added during testing — status reporting in
`anvil deploy`.

Verification on each step used:

- `anvil deploy` output (from round 2, the per-item database results)
- the bootstrap Lambda's CloudWatch logs
- admin SQL against the cluster: `pg_roles` (with role comments),
  `information_schema.role_table_grants`, `pg_default_acl`,
  `sys.iam_pg_role_mappings`, `pg_attribute` / `format_type`, `pg_indexes`,
  `sys.jobs`
- invoking a Rust Lambda (`seedOrders`) that connects as `orders_writer` and
  inserts rows, to prove real access end to end

## 2. How testing was run

| Round | What | Code |
|---|---|---|
| 1 | Full plan (sections 1–6 below) on one app, one change per deploy | `dsqlVPC` as found, plus the race fix part-way |
| 2 | Failing round-1 cases re-run after fixes | Fixes for bugs 1–4, status items, role cleanup |
| 3 | Real upgrade: fresh deploy on the old code, then in-place upgrade | Old: `dsqlVPC` last commit (`95afef78`) built in a worktree. New: working tree |
| 4 | Remaining round-1 cases re-run, 5 apps in parallel | Working tree with all fixes |

Legend: ✅ pass · ❌ failed (bug) · 🔧 failed, fixed, re-verified on AWS · ⏭ not re-run after later code changes

---

## 3. Results by case

### Section 1 — Baseline (fresh deploy)

| Case | Round 1 | Later |
|---|---|---|
| Fresh deploy, scoped role + 4 tables | ❌ Role and table records ran in parallel; `GRANT` hit 42P01, `CREATE TABLE` hit OC001; the role's record exhausted retries. Deploy green, no grants, `seedOrders` failed | 🔧 Race fix. Verified on 2 fresh deploys (one hit 4 conflicts, all recovered), then on every fresh deploy after |
| Fresh deploy with status reporting | — | ✅ Every item reported `applied`; sweep: "every role matches your code"; `seedOrders` inserts |
| Redeploy with no changes | ✅ No stream events | ✅ No stream events; existing failures/drift still printed |

### Section 2 — Changing a role's grants

| Case | Round 1 | Round 4 (`dsql-grants`) |
|---|---|---|
| 2a Add `UPDATE` | ✅ | ✅ Status shows new grants; `UPDATE` on both tables; role still tagged |
| 2b Remove `INSERT` | ✅ App denied | ✅ `INSERT` revoked; app gets `permission denied for table orders` |
| 2b Restore `INSERT` | ✅ | ✅ App inserts again |
| 2c `grants: []` | ✅ Rejected at deploy | ⏭ (provider validation, unchanged) |

### Section 3 — Table scoping

| Case | Round 1 | Round 4 (`dsql-scoping`) |
|---|---|---|
| 3a New table, not scoped | ✅ No access | ⏭ |
| 3b Scoped → schema-wide | ✅ | ✅ All 4 tables + default privileges `orders_writer=ar/admin`; status says schema-wide |
| 3c New table while schema-wide | ✅ | ✅ Created and covered automatically |
| 3d Schema-wide → scoped | ✅ | ✅ Back to the 2 scoped tables; default privileges removed; app inserts |
| 3e New table + scope in one deploy | ✅ Role ran first, table record granted it | ✅ Also exercised by every fresh deploy |
| 3f Scope an undeclared table | ✅ Rejected at deploy | ⏭ (provider validation, unchanged) |
| 3g Remove a table still scoped | ✅ Rejected at deploy | ⏭ (provider validation, unchanged) |

### Section 4 — Role lifecycle

| Case | Round 1 | Later |
|---|---|---|
| 4a Add a second role and map it | ✅ | ✅ Round 2 (`temp_role`), with statuses |
| 4b Remove `grantConnect` only | ✅ Mapping revoked, role kept | ⏭ |
| 4c Remove a role | ✅ | ✅ Round 2: mapping revoked, then dropped |
| 4d Remove a role still granted | ❌ `DROP ROLE` failed (2BP01), swallowed; role left, still mapped | 🔧 Deploy now fails (strict `grantConnect`); removal revokes mappings before dropping |
| 4e Rename a role (and back) | ✅ | ✅ Round 4 (`dsql-rename`): only the new role remains, tagged and mapped; renaming back restores the original |
| 4f `grantConnect` to an undeclared role | ❌ Deploy green, grant failed silently, IAM policy left | 🔧 Deploy fails, lists the declared roles; no policy created |
| 4f+ `grantConnect` naming the wrong schema | ❌ Schema ignored | 🔧 Deploy fails: "declared in schema shop, not billing" (round 2 and round 4) |
| 4g Same role name in two schemas | ✅ Rejected at deploy | ⏭ (provider validation, unchanged) |
| 4h Move a role to another schema | ✅ | ✅ Round 4 (`dsql-move`): only `billing.invoices`; USAGE moved; mapping kept; still tagged |
| Stray role Anvil created (tagged) | — | ✅ Dropped by the sweep |
| Stray role not created by Anvil | ❌ Left forever | ✅ Reported by default; dropped with `pruneUnmanagedRoles: true`; with it off again: "every role matches your code" |

### Section 5 — Schema changes

| Case | Round 1 | Later |
|---|---|---|
| 5a Add a nullable column | ✅ | ✅ Round 2 |
| 5b Add `NOT NULL`/`DEFAULT` column to an existing table | ❌ DSQL rejects (0A000); deploy green, column never added, app broke | 🔧 Deploy fails with DSQL's limit (see §5). Anvil follows DSQL; no workaround |
| 5b follow-on: fix the config | ❌ Never retried (diff was old item vs new item) | 🔧 Diff now uses the real catalog; column added next deploy |
| 5c Add an index | ✅ Async job completed | ✅ Covered by fresh deploys |
| 5d Remove a column from code | ✅ Left in the database | ⏭ Covered by unit test |
| 5e Re-add a column with a different type | ❌ Logged `SQL OK`, nothing changed; app failed at runtime | 🔧 Reported as drift; cleared when config matches |
| 5f Change an existing column's type/default | ✅ Ignored by design, unreported | 🔧 Type and nullability differences reported as drift (defaults not compared) |
| 5g Rename a table | ✅ New table created, old kept | ⏭ Create path covered by fresh deploys |
| 5h Change the primary key | ✅ Ignored | ⏭ Covered by unit test |
| 5i New schema + role + table in one deploy | ✅ (with 4h) | ✅ Round 4 (`dsql-move` setup) |
| 5j `public` schema | ✅ Rejected | ⏭ (provider validation, unchanged) |
| 5j More than 10 schemas | Not run | Not run |

### Section 6 — Security and failure handling

| Case | Round 1 | Later |
|---|---|---|
| 6a Forged invoke, fake stream ARN | ✅ Rejected | ⏭ |
| 6a Forged INSERT for an item not in the table (incl. mapping an attacker ARN) | ✅ Skipped | ⏭ |
| 6b Forged REMOVE for a role that exists | ✅ Skipped | ⏭ |
| 6c Injected grant written to the table | ✅ Rejected by allowlist | ✅ Round 4: rejected, recorded as a `failed` status, no role created, `public` gained nothing; 🔧 next deploy now reports it and fails |
| 6d Injected column default | ✅ Rejected | ⏭ |
| 6e Role item deleted by hand | ❌ Grants revoked, drop failed (2BP01); plain deploy "No changes"; `--refresh` repaired | 🔧 Round 4: role dropped cleanly with its mapping; plain deploy still "No changes"; `--refresh` recreates the role 🔧 **and its mapping** (fixed in round 4). App then got `permission denied for schema shop` on a warm connection — see §6 |
| 6f Record that keeps failing | ✅ Dropped after 4 attempts; stream moves on | ✅ Permanent failures now go straight to a `failed` status |
| 6g `anvil destroy` | ✅ With `--stage --force` | ✅ Used throughout |

### Upgrade path (round 3)

| Check | Result |
|---|---|
| Fresh deploy on the old code | 4 tables, `orders_writer` granted, no item versions |
| Upgrade deploy on the branch, no manual replay (first attempt) | ❌ Lambda updated in parallel with item writes → 🔧 fixed. Then the IAM mapping's MODIFY was ignored → 🔧 fixed |
| Upgrade deploy (final, from scratch) | ✅ All 7 items reported (mapping, role, sweep, 4 tables) |
| Records handled by old code after the Lambda update | ✅ 0 (all 6 old-code lines were from the old deploy) |
| New-code status writes after the update | ✅ 8 |
| App after upgrade | ✅ `seedOrders` inserts |

### Round 4 totals

| App | Cases | Result |
|---|---|---|
| `dsql-scoping` | 3b, 3c, 3d | 14 / 14 |
| `dsql-rename` | 4e | 11 / 11 |
| `dsql-move` | 4h (+ wrong-schema grant) | 10 / 10 |
| `dsql-grants` | 2a, 2b | 12 / 13 — stale role status message (fixed afterwards, see §4 #8) |
| `dsql-security` | 6c, 6e | 12 / 13 — warm connection after role re-creation (see §6) |

---

## 4. Bugs found and fixed

| # | Bug | Fix | Verified on AWS |
|---|---|---|---|
| 1 | Role and table records race on fresh deploys | OC001 retried in the Lambda with jittered backoff; scoped grants applied by whichever record runs second; `CREATE SCHEMA` only when missing | ✅ |
| 2 | Column with `NOT NULL`/`DEFAULT` added to an existing table fails silently | Rejected with DSQL's reason; deploy fails | ✅ |
| 3 | Failed column add never retried; type changes unreported | Table changes compared against the real catalog; mismatches reported as drift | ✅ |
| 4 | Removing a role still mapped leaves it behind | Removal revokes all IAM mappings first; roles tagged `managed-by:anvil`; sweep drops tagged strays, reports or (`pruneUnmanagedRoles`) drops untagged ones | ✅ |
| 5 | `grantConnect` to an undeclared role / wrong schema accepted | `DSQLConnect` validates against `declaredRoles`; deploy fails | ✅ |
| 6 | Deploy green while the database is wrong | Status items in the roles table; `anvil deploy` waits, prints results, fails on `failed` | ✅ |
| 7 | Upgrade: Lambda updated in parallel with item writes; mapping MODIFY ignored | Item writes and `rolesTableName` wait for the Lambda, its policy and its trigger; mapping MODIFY re-applies | ✅ |
| 8 | Role re-created without its IAM mapping (6e) | Creating a role re-applies its mappings from the table | ✅ |
| 9 | Injected items (no `version`) not reported by deploy | CLI reports `failed`/`drift` statuses on unversioned items | ✅ |
| 10 | Role status stays "waiting for scoped tables" after tables are granted | Table records rewrite the role's status; both writers read the catalog at write time | ⚠️ Unit tests only — **not re-run on AWS** |

## 5. DSQL limit confirmed

Adding a column to an existing table: DSQL's `ALTER TABLE ADD COLUMN` accepts
only a name and a type. Probed directly on a cluster:

| Statement | Result |
|---|---|
| `ADD COLUMN c1 text NOT NULL DEFAULT 'web'` | ❌ `ALTER TABLE ADD COLUMN with constraint not supported` |
| `ADD COLUMN c1 text DEFAULT 'web'` | ❌ same |
| `ADD COLUMN c1 text` | ✅ |
| `ALTER COLUMN c1 SET DEFAULT 'web'` | ✅ (new rows only) |
| `ALTER COLUMN c1 SET NOT NULL` | ❌ unsupported |
| `ADD CONSTRAINT … CHECK (c1 IS NOT NULL) NOT VALID` + backfill + `VALIDATE` | ✅ |

Decision: Anvil follows DSQL's limits and does not work around them; the deploy
fails with the reason. Documented in the DSQL docs.

Also confirmed: `COMMENT ON ROLE` works (used for the `managed-by:anvil` tag),
and `AWS IAM GRANT` of an existing mapping is a no-op notice.

## 6. Known issues (not fixed)

| Issue | Evidence |
|---|---|
| Every schema/grant change makes each warm app connection fail once with OC001 (`40001`); the generated `gen/db` client doesn't retry | Seen in every app; controlled experiment: one failure, then recovers on the same connection |
| After a role is dropped and re-created, warm connections stay broken until they reconnect | 4e: OC001 on both tries, fresh connection worked (confirmed). 6e: `permission denied for schema shop` on both tries with role, grants and mapping all present — likely the same cause, **not confirmed** with a cold start |
| Grant changes revoke everything then re-grant in separate transactions — a brief window where the role has no privileges | By construction; not observed as a lasting problem |
| A role item deleted by hand isn't flagged by a plain deploy ("No changes"); `--refresh` repairs it | 6e |
| Generated Rust insert struct breaks on every new column (`E0063`); app errors only say `db error` | Seen on every column change |
| `DSQLConnect` validation errors are prefixed with Pulumi internals; CLI prints "Deploy failed — 0 failed" | 4f, 4h |
| Drift checks don't compare column defaults | By design of the fix |

## 7. Not tested

- More than 10 schemas (5j)
- Fix #10 on AWS
- 6e with a cold start after `--refresh`
- Cases marked ⏭ after the code they touch changed: 2c, 3a, 3f, 3g, 4b, 4g, 5d, 5g, 5h, 5j, 6a, 6b, 6d (provider-validation cases are unaffected; the others rely on unit tests or related cases)
- Multi-region clusters and VPC endpoints with the new behaviour
- Python and Go SDKs on AWS (generated and compiled only; all AWS runs used TypeScript)

## 8. Unit tests

All passing at the end of testing:

- `cmd/anvil/dsql-lambda`: verify, SQL allowlists, OC001 retry and backoff,
  scoped grant statements, roles-scoped-to scan, mapping lookup, catalog type
  names, table change planning (add / reject / drift / PK / removed columns),
  status outcomes, role classification, grant messages, retry classification,
  status-record detection
- `provider/aws/dsql`, `provider/aws/dsqlgrant`: role validation (undeclared,
  wrong schema, none declared), item version stability
- `cmd/anvil/cmd`: status pairing, unversioned failed items, item labels,
  failure count

## 9. Environment at the end

All test stacks destroyed (testForwardRef and the five `dsql-rerun` apps), all
16 test state buckets deleted. Remaining: `testing-cdcPrinter-lambda-25bef3`,
an orphan from before testing. Full `go run ./build build` passed; the plugin
cache has the final provider, `/usr/local/bin` was last installed before fix #10.
