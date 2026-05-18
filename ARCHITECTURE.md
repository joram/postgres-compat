# Architecture

postgres-compat is a single Go binary that runs three independent test engines against one target PostgreSQL endpoint and aggregates their outcomes into one structured report. The engines are:

- **`pg_regress` wrapper** — drives PostgreSQL's own `src/test/regress` corpus inside a `postgres:<major>` Docker container.
- **`pg_isolation_regress` wrapper** — same shape, for `src/test/isolation`.
- **YAML harness** — declarative probe/validate tests for things only this project can express (extension gating, runtime config probes, replication objects with per-run names, target introspection, contrib-extension behaviour).

Every engine produces `Result` records sharing a common `TestCase` shape. [main.go](main.go) merges them, applies the report-level filters uniformly, and writes the report.

## Run order

The harness executes the engines in a deliberate order: **regress engines first, then harness setup, then YAML probes**. This is non-obvious and load-bearing — `pg_regress` and `pg_isolation_regress` expected outputs are sensitive to extra catalog entries (extensions add operators, types, namespaces) and to non-default `search_path`. If we provisioned our test schema and extensions before running the upstream suites, we'd inject diff noise into them.

Concretely, [run() in main.go](main.go) does:

1. **Pre-flight** — open pgxpool; abort if the target already has user objects in non-system schemas (`-allow-dirty` to skip).
2. **`pg_regress`** — Docker-driven, against the target with its initial state.
3. **`pg_isolation_regress`** — same shape.
4. **Preload phase (optional)** — if the suite references contrib extensions whose user-facing SQL surface refuses to work without `shared_preload_libraries` (currently just `pg_stat_statements`) and `-docker-container <name>` was passed: read current `shared_preload_libraries`, union in the needed entries, `ALTER SYSTEM SET`, run `docker restart <name>`, wait for the target to come back, recreate the pool. Without `-docker-container` the harness logs a one-line note and proceeds; affected tests then fail with `ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE` (`55000`).
5. **`CREATE SCHEMA pgcompat_<runid>`** — set as `search_path` first entry on every pool connection via `AfterConnect`. Dropped `CASCADE` at run end.
6. **`provisionExtensions`** — for each distinct extension declared by YAML tests, attempt `CREATE EXTENSION`; record availability. Tests whose extension didn't take are `Skipped`, never failed. Extensions the harness installed fresh are dropped at run end.
7. **YAML probes** — concurrency-controlled goroutines, `serial: true` tests hold a write lock.
8. **Report aggregation + write**, then deferred cleanup (LIFO: drop extensions, drop schema, close pool).

## Multi-version corpus selection

The user asserts the version under test via `-pg-version`. There is no autodetection from `server_version_num` — the target's reported version is recorded but never feeds back into corpus selection.

[regress.go:`corpusFor`](regress.go) maps a major to a corpus descriptor: docker image, `pg_regress` and `pg_isolation_regress` binary paths, and source directories. Both engines look up the same descriptor.

The on-disk corpora are pulled from the postgres git repo by `make upstream-<major>`. Each clone is pinned to the `postgres:<major>` image's actual minor — bug fixes occasionally land in a minor with the relevant regression test updated alongside, and corpus and binary must agree.

`-pg-version` is validated against `corpusFor` at startup; an unsupported value is a hard error before any work runs. A missing corpus dir produces a warn-and-skip from each regress wrapper.

## YAML harness

Tests are declarative YAML files under `suite/<domain>/<topic>.yaml`. **The directory tree IS the taxonomy** — the directory name is the domain, the filename stem is the topic. Nothing is hardcoded in Go.

[loader.go](loader.go) parses each file into `TestCase` structs ([types.go](types.go)). Each test has up to four phases: `setup`, `probe`, `validate`, `teardown`. The probe runs the operation under test; the validate step asserts observable state. The split is what catches **silent failures**: a probe that succeeds but a validate that shows the operation didn't take effect.

Concurrency: a buffered semaphore caps in-flight tests; tests that need to run alone hold a write lock against a `RWMutex` shared by all the others. Run-level isolation: every YAML run gets its own per-run schema (`pgcompat_<runid>`), set as the `search_path` first entry on every pool connection, dropped `CASCADE` at the end.

**SQL placeholders** (replaced by [`expandSQL` in main.go](main.go) before each statement runs):

- `{runid}` — per-run schema ID. Used for database-global objects (roles, top-level schemas) so concurrent runs against the same target don't collide.
- `{dbname}` — target database name from the connection URL. Used by tests that need to identify the current DB by name (e.g. `postgres_fdw` self-loopback's `CREATE SERVER … OPTIONS (dbname '{dbname}')`).
- `{user}` — connecting role from the connection URL. Used for sub-connections that need to authenticate as the same role (e.g. `postgres_fdw`'s `CREATE USER MAPPING … OPTIONS (user '{user}')` — libpq's default OS user almost never matches the connecting role).

## `pg_regress` wrapper

[regress.go:`runPgRegress`](regress.go) drives the upstream regress corpus through `pg_regress` running inside the matching `postgres:<major>` Docker image, networked to the target. Three steps:

**(a) Build the input directory.** Copy the upstream `src/test/regress` tree into a temp dir and write one extra file: `pgcompat_schedule`, a filtered version of upstream's `parallel_schedule` with out-of-scope test lines stripped. Upstream `.sql` and `.out` files are never modified — only the schedule (which is ours to compose) is filtered.

The `regressOutOfScope` set covers tests that exercise PostgreSQL's *own* source-level invariants rather than user-facing SQL surface, so they don't belong in a vendor compatibility harness:

- `test_setup` — replaced by step (b) below
- `opr_sanity`, `type_sanity`, `misc_sanity`, `sanity_check`, `oidjoins` — catalog/operator/type sanity for PG itself
- `create_function_c` — tests the C-extension loading mechanism (`regress.so`)
- `psql`, `psql_crosstab` — test the psql client, not the server

**(b) Preload fixture tables over libpq.** Upstream `test_setup.sql` can't run as-is against an arbitrary target: server-side `COPY FROM` needs files on the *server's* filesystem, and its `LANGUAGE C` declarations need `regress.so` (a developer-only artifact). [`preloadRegressFixtures`](regress.go) reads `test_setup.sql` line by line, runs inline DDL/DML via `pgx.Conn.Exec`, and streams the upstream `data/*.data` files into the fixture tables via `pgconn.CopyFrom` using the standard COPY FROM STDIN protocol. No psql, no Docker for fixture loading. Statement errors (`CREATE TABLESPACE` without `allow_in_place_tablespaces`, `CREATE FUNCTION ... LANGUAGE C` without `regress.so`) are logged at debug and skipped — the tests that would call those C functions are out of scope.

**(c) Run `pg_regress`** against the filtered schedule with `--use-existing` (no createdb/dropdb against the target). The actual Docker invocation lives in the shared [`runDockeredEngine`](regress.go) helper, which both regress engines call. TAP output is parsed by the shared [`parseTAPOutput`](regress.go); test name → `(domain, topic, core)` mapping comes from [regress_topics.go](regress_topics.go); unmapped names fall through to a default `behavior_and_core_experience/sql_regression` Core mapping.

Cleanup is *not* attempted on the target — fixture tables stay in `public` after the run. `pg_regress` was designed for fresh databases; subsequent runs against the same DB will see leftover state and diff. The fresh-DB pre-flight in step 1 of the run order enforces this contract.

## `pg_isolation_regress` wrapper

[isolation_regress.go](isolation_regress.go) is the same shape but simpler: no fixture preload, no schedule filtering — the upstream isolation suite has no PG-internal sanity tests. Uses the same `runDockeredEngine` and `parseTAPOutput` helpers. Specs all map to a single `behavior_and_core_experience/transaction_isolation` topic.

## Result aggregation and report

All three engines emit `Result` into a shared slice. Domain/topic ordering follows the YAML suite directory; pg_regress topics not represented there sort after, alphabetically. Output is YAML.

Exit code: 1 if any Core Result failed; 0 otherwise. Optional failures never affect exit code — they're reported with full diagnostic data so derivative authors can target them as stretch goals.

## Cross-cutting invariants

- **Run all, record all, never abort.** A single test failing does not stop the run. Every test produces a Result; the report is always complete.
- **Regressions run against an unmodified target.** No harness-installed extensions or schemas exist when `pg_regress` / `pg_isolation_regress` execute. This is enforced by the run order — extensions and the run-level schema are provisioned only after both regress engines complete.
- **No upstream modification on disk.** The regress corpora are read-only mounts. Patching happens in memory or by writing *new* files alongside the originals (e.g. our filtered `pgcompat_schedule` next to upstream's `parallel_schedule`).
- **No autodetection of target version.** `-pg-version` is the user's assertion; the target's reported version is recorded but never used to choose corpus.
- **Per-run schema isolation only for YAML.** `pg_regress` and `pg_isolation_regress` connect outside our pgxpool and run in the database's default search_path. They're scheduled in a fresh DB by contract; the fresh-DB pre-flight check enforces it.
- **Pass/fail is binary.** `Result.Passed()` is a bool; weighting between tests is qualitative (Core gates the exit code, Optional doesn't), not a per-test fractional score.
- **Docker is the only host dependency for the regress engines and the optional restart.** `pg_regress`, `pg_isolation_regress`, fixture preload via libpq, and `docker restart <name>` (when `-docker-container` is set) all need Docker on PATH; nothing else from the host PostgreSQL packages.

## File map

| Path | Role |
|---|---|
| [main.go](main.go) | CLI; pre-flight fresh-DB check; pgxpool; engine orchestration in run-order (regressions → preload phase if `-docker-container` → run-level schema → extension provisioning → YAML scheduler); SQL placeholder expansion; report aggregation; output. |
| [types.go](types.go) | `TestCase`, `Result`, `Passed()` bool, version-gating, validate-helper closures. |
| [loader.go](loader.go) | Walks `suite/`, parses YAML into `TestCase`s, builds `Validate` closures, computes domain/topic IDs from filesystem. |
| [regress.go](regress.go) | `pg_regress` wrapper, corpus deriver, schedule filter, libpq-only fixture preload. Hosts the shared `runDockeredEngine` and `parseTAPOutput` helpers used by both regress engines. |
| [isolation_regress.go](isolation_regress.go) | `pg_isolation_regress` wrapper. Calls into `runDockeredEngine` / `parseTAPOutput` in regress.go. |
| [regress_topics.go](regress_topics.go) | Compact map: which upstream regress tests are legitimately Optional (need non-default GUCs, build flags, extensions, `wal_level=logical`, etc). Everything else falls through to a default Core mapping. |
| [Makefile](Makefile) | `upstream-<major>` targets: query the docker image's minor, pull the matching tag from postgres git. |
| [suite/](suite/) | YAML test corpus. Directory tree IS the report taxonomy. |
| [upstream/](upstream/) | Gitignored. Per-major sparse-shallow checkouts of the postgres source tree. |
| [compat_test.go](compat_test.go) | Unit tests for the harness's pure-Go pieces. |
