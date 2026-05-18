# postgres-compat

A compatibility harness that drives PostgreSQL's own regression suites against any target and maps results to the [PGConf.EU 2025 Riga consensus](https://wiki.postgresql.org/wiki/PGConf.EU_2025_Establishing_the_PostgreSQL_standard_What_is_Postgres_compatible) taxonomy.

The test corpus is not maintained here. It is delegated to two community sources:

1. **PostgreSQL upstream** — `pg_regress` and `pg_isolation_regress` are run verbatim against the target: 200 regression test files + 117 isolation specs, byte-equivalent output comparison.
2. **PGConf.EU 2025 Riga consensus** — results are mapped to the three domains agreed at PGConf.EU 2025 (*Behavior & Core Experience*, *Functionality*, *Backups & Replication*), with explicit **Core** vs **Optional** distinction.

This is a reference implementation of that framework, proposed as a starting point at [PGConf.dev 2026 session 450](https://2026.pgconf.dev/session/450) — *"Establishing the PostgreSQL Standard: What's Postgres compatible?"* (Angelakos & Dombrovskaya).

Test concepts derived from the [postgres-compatibility-index](https://github.com/secp256k1-sha256/postgres-compatibility-index) reference (MIT). All Go code is original work under the PostgreSQL License.

## How it works

Three test engines, one structured report:

- **`pg_regress`** — PostgreSQL's SQL-surface regression suite, one corpus per supported major (PG14–18). Runs inside the matching `postgres:<major>` Docker image.
- **`pg_isolation_regress`** — PostgreSQL's concurrent-session test runner, same execution model.
- **YAML probes** — what the upstream suites cannot express against an arbitrary target: runtime config visibility, WAL and backup primitives, replication objects, target introspection, contrib-extension behaviour, and probe/validate pairs that report cases where a target accepts a statement (e.g. `ALTER SYSTEM SET …`) but the change is not reflected in observable state.

**Run order.** The harness deliberately runs the upstream regress engines *before* it provisions its own schema or extensions — `pg_regress` and `pg_isolation_regress` expected outputs are sensitive to extra catalog entries (extensions add operators, types, namespaces) and to non-default `search_path`. So:

1. fresh-DB pre-flight check
2. `pg_regress`
3. `pg_isolation_regress`
4. (optional) ALTER SYSTEM SET `shared_preload_libraries` + restart, if the suite references extensions that need it (currently just `pg_stat_statements`) and `-docker-container` was passed
5. `CREATE SCHEMA pgcompat_<runid>`, `CREATE EXTENSION` for each declared contrib extension
6. YAML probes
7. Cleanup: `DROP EXTENSION`, `DROP SCHEMA … CASCADE`

Upstream `.sql` / `.out` / `.spec` files run verbatim. The schedule we hand `pg_regress` strips a small set of tests that probe PostgreSQL's own source-level invariants (`opr_sanity`, `type_sanity`, C-extension loading, …) — those are not user-facing compatibility signals. Tests requiring server-side filesystem access are likewise out of scope: they are restricted on most managed services.

Permission-restricted tests are reported separately from missing features: a `42501` (insufficient privilege) error appears in the report with `sql_state: 42501` rather than as an unqualified failure.

## Usage

```bash
# Run against a local database
go run . -url "postgres://user:pass@localhost:5432/db?sslmode=disable"

# Or set PGURL and omit -url
PGURL="postgres://..." go run .

# Managed service (RDS, AlloyDB, Neon, CrunchyBridge, Yugabyte…)
go run . -url "postgres://app_user:****@my-cluster.example.com:5432/db?sslmode=require"

# Run only Core tests
go run . -url "..." -core-only

# Filter by domain or topic (substring match, case-insensitive)
go run . -url "..." -category "Indexing"
go run . -url "..." -category "behavior"

# Verbose per-test output
go run . -url "..." -log-level debug
```

The report is written to `report.yaml` (`-out` to change). Exit code `1` if any **Core** test failed, `0` otherwise.

## Running pg_regress and pg_isolation_regress

Both engines run **by default** and require Docker plus a one-time corpus pull (~30 MB per major). Pass `-no-pg-regress` or `-no-isolation-regress` to skip them (e.g. while iterating on YAML probes locally). If Docker or the corpus is missing, the engines emit a warning and skip — they don't fail the run.

```bash
make upstream        # PG14–18
make upstream-18     # one major
make clean && make upstream  # refresh to latest minor
```

Run against a target:

```bash
go run . -url "postgres://..."

# Skip the upstream suites (YAML probes only, fast iteration)
go run . -url "..." -no-pg-regress -no-isolation-regress

# Assert a specific PG major (default: 18)
go run . -url "postgres://..." -pg-version 17
```

`-pg-version` is what the user is asserting compatibility against — it is not autodetected from the target's `server_version_num`. A vendor product may report one version string while the user is testing whether it matches the behavior of another.

Fixture tables (`onek`, `tenk1`, …) are preloaded from upstream `data/` files over libpq using the standard `COPY … FROM STDIN` protocol, so no server-side file access is required. Both runners execute inside the matching `postgres:<major>` Docker image — no host PostgreSQL installation.

### Fresh database required

`pg_regress` is designed for fresh databases. The harness uses `--use-existing` (no `createdb`/`dropdb` against the target) so it works against managed services where the app role lacks CREATEDB privilege — but this means **the user is responsible for handing it a clean DB**. Re-running against a populated database produces spurious diffs in `fast_default`/`select_parallel`/`vacuum_parallel` from leftover fixture state.

The harness aborts before the run if the target has user objects in non-system schemas. Recommended pattern:

```bash
createdb pgcompat_run
PGURL="postgres://…/pgcompat_run" go run .
dropdb pgcompat_run
```

Pass `-allow-dirty` to skip the pre-flight check if you know the leftover state is harmless.

### Extensions that need shared_preload_libraries

Some contrib extensions — currently `pg_stat_statements` — refuse to work unless they're loaded via `shared_preload_libraries` *at server start*. CREATE EXTENSION alone doesn't suffice; the SRF C functions check at execution time and emit `ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE` (SQLSTATE `55000`) if the library isn't loaded.

#### Self-hosted Docker target

Pass `-docker-container <name>` and the harness handles the preload cycle automatically. Between the regression engines and the YAML probes it:

1. reads the target's current `shared_preload_libraries`,
2. unions in the preload-needing extensions referenced by the suite,
3. `ALTER SYSTEM SET shared_preload_libraries = …`,
4. runs `docker restart <name>`,
5. waits for the target to come back, recreates the connection pool.

```bash
go run . -url "..." -pg-version 18 -docker-container <your-container-name>
```

#### All other targets (managed services, Kubernetes, systemd, bare metal)

The harness has no provider-API integration and cannot initiate restarts on targets it doesn't directly control. **Configure `shared_preload_libraries` and restart out-of-band before running the harness**, then invoke it with no `-docker-container` flag:

- **Managed services** (RDS, Aurora, AlloyDB, Neon, CrunchyBridge, …): enable `pg_stat_statements` via the provider's parameter system (AWS parameter group, GCP database flags, Azure server parameters, etc.) and trigger a maintenance restart through their console/API.
- **Kubernetes**: update your `postgresql.conf` (Helm values / ConfigMap / kustomize patch) and roll the StatefulSet.
- **systemd / bare metal**: edit `postgresql.conf` and `systemctl restart postgresql`.

If the library isn't preloaded when the harness runs, the affected tests fail with SQLSTATE `55000`. The report records this with the specific error code, so a reader can distinguish a deployment-config gap (operator hasn't enabled the extension) from a real vendor incompatibility (provider doesn't ship the extension at all).

## YAML probes

Tests live under `suite/<domain>/<topic>.yaml` — the directory tree is the taxonomy, nothing is hardcoded in Go.

```yaml
- id: my-new-test
  description: "What this tests"
  core: true
  setup:
    - "CREATE TABLE t_foo (id INT)"
  probe:
    - "INSERT INTO t_foo VALUES (1)"
  validate:
    returns_value:
      sql: "SELECT count(*) FROM t_foo"
      equals: "1"
  teardown:
    - "DROP TABLE t_foo"
```

**Validate options** (one per test): `returns_value`, `explain_uses`, `explain_contains`. A `validate.setup` list runs SQL before the check (e.g. `ANALYZE`, `SET enable_seqscan = off`).

**Version-gating:** `supported_versions: [15, 16, 17]` limits a test to specific PG majors; omit for all versions.

**Negative tests:** `expect_error: "23505"` instead of `validate` when the probe is expected to fail with a specific error code.

**SQL placeholders** in any setup/probe/validate/teardown string:

- `{runid}` — per-run schema ID. Use for database-global objects so concurrent runs against the same target don't collide (`CREATE ROLE t_role_{runid}`).
- `{dbname}` — target database name parsed from the connection URL. Used by tests that need to identify the current DB by name (e.g. `postgres_fdw` self-loopback).
- `{user}` — connecting role name parsed from the connection URL. Used by tests that need to authenticate sub-connections as the same role (e.g. `postgres_fdw` user mapping).

## Output

`report.yaml` (schema_version: 2):

```yaml
schema_version: 2
generated_at: 2026-04-26T13:45:00Z
target:
  server_version: "17.9 (Debian 17.9-1.pgdg13+1)"
  server_version_num: 170009
  server_major: 17
run:
  parallel: 50
  duration_ms: 103
summary:
  core_total: 56
  core_passed: 56
  optional_total: 3
  optional_passed: 2
  skipped: 0
  silent_failures: 0
domains:
  - id: behavior_and_core_experience
    topics:
      - id: sql_compliance
        tests:
          - id: ddl-schemas
            core: true
            passed: true
            duration_ms: 8
            silent_failure: false
```

`passed: true/false` is binary by design — Core vs. Optional is the only weighting (Core failures fail the gate; Optional failures don't). `silent_failure: true` means the probe succeeded but the validate query showed the operation did not take effect. `sql_state` is populated on permission-denied (`42501`) and other PG errors.

## Flags

| Flag | Default | Description |
|---|---|---|
| `-url` | `$PGURL` | libpq connection string |
| `-out` | `report.yaml` | Output path |
| `-parallel` | `50` | Max concurrent tests |
| `-timeout` | `30s` | Per-test timeout |
| `-core-only` | false | Skip Optional tests |
| `-category` | | Substring filter on domain or topic name (case-insensitive) |
| `-log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `-no-pg-regress` | false | Skip upstream `pg_regress` (default: run it; needs Docker + `make upstream-<major>`) |
| `-no-isolation-regress` | false | Skip upstream `pg_isolation_regress` (same requirements) |
| `-pg-version` | `18` | PG major to assert: `14`, `15`, `16`, `17`, or `18` |
| `-allow-dirty` | false | Skip the fresh-DB pre-flight check |
| `-docker-container` | | Name of the target's Docker container; enables ALTER SYSTEM + `docker restart` so `shared_preload_libraries`-needing tests can run |
| `-version` | | Print version and exit |

## Status and contributing

This is a reference implementation, not a ratified standard. The PGConf.EU 2025 Riga consensus produced the taxonomy framework; the aim of PGConf.dev 2026 session 450 is to refine and extend it, using this harness as the starting point for the conversation.

**Core vs Optional criteria:** *Core* covers behaviour any PostgreSQL user reasonably expects to work on a stock install. *Optional* covers features that depend on superuser privileges, specific GUC settings, or extensions.

**Feature compliance, not bug-for-bug:** what `pg_regress` exercises is core PostgreSQL behaviour — vendors and derivatives are expected to match it. The "not bug-for-bug" carve-out is narrow: if upstream ships a bug in major X, a derivative is not required to import that bug to claim X compatibility. It does not relax expected core behaviour.

- New tests, corrections to Core/Optional designations, and taxonomy refinements: GitHub issues.
- Vendor reports: publish a `report.yaml` against a tagged release alongside product documentation.

## License

PostgreSQL License.
