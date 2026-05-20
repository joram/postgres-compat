package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

type Report struct {
	SchemaVersion int       `yaml:"schema_version"`
	GeneratedAt   time.Time `yaml:"generated_at"`
	Target        struct {
		ServerVersion        string `yaml:"server_version"`
		ServerVersionNum     int    `yaml:"server_version_num"`
		ServerMajor          int    `yaml:"server_major"`
		PrimaryServerVersion string `yaml:"primary_server_version,omitempty"`
	} `yaml:"target"`
	Run struct {
		Parallel   int   `yaml:"parallel"`
		DurationMS int64 `yaml:"duration_ms"`
	} `yaml:"run"`
	Summary struct {
		CoreTotal      int `yaml:"core_total"`
		CorePassed     int `yaml:"core_passed"`
		OptionalTotal  int `yaml:"optional_total"`
		OptionalPassed int `yaml:"optional_passed"`
		Skipped        int `yaml:"skipped"`
		SilentFailures int `yaml:"silent_failures"`
	} `yaml:"summary"`
	Domains []DomainResult `yaml:"domains"`
}

type DomainResult struct {
	ID     string        `yaml:"id"`
	Topics []TopicResult `yaml:"topics"`
}

type TopicResult struct {
	ID    string       `yaml:"id"`
	Tests []TestOutput `yaml:"tests"`
}

type TestOutput struct {
	ID            string `yaml:"id"`
	Core          bool   `yaml:"core"`
	Alter         bool   `yaml:"alter"`
	Passed        bool   `yaml:"passed"`
	DurationMS    int64  `yaml:"duration_ms"`
	SilentFailure bool   `yaml:"silent_failure"`
	SQLState      string `yaml:"sql_state,omitempty"`
	Reason        string `yaml:"reason,omitempty"`
}

var (
	runID               string
	targetDB            string // dbname from the connection URL; exposed in YAML SQL via {dbname}
	targetUser          string // user from the connection URL; exposed in YAML SQL via {user}
	availableExtensions map[string]bool
	readOnlyTarget      bool         // true when target is read-only (hot standby or default_transaction_read_only)
	primaryPool         *pgxpool.Pool // non-nil when -primary-url is set; alter:true tests run here
	version             = "dev"
)

func main() {
	os.Exit(run())
}

// run is the real entrypoint; main is a thin wrapper so deferred cleanups
// (DROP SCHEMA, DROP EXTENSION, pool close) actually execute before exit —
// os.Exit skips defers, but a normal return from run() doesn't.
func run() int {
	var (
		urlStr               = flag.String("url", os.Getenv("PGURL"), "Database connection string")
		primaryURL           = flag.String("primary-url", os.Getenv("PG_PRIMARY_URL"), "Primary (read-write) connection string. When set, alter:true tests run against the primary and alter:false tests run against -url. Useful for testing a read replica against its primary.")
		outPath              = flag.String("out", "report.yaml", "Output YAML path")
		parallel             = flag.Int("parallel", 50, "Max parallel tests")
		timeout              = flag.Duration("timeout", 30*time.Second, "Per-test timeout")
		coreOnly             = flag.Bool("core-only", false, "Only run core tests")
		category             = flag.String("category", "", "Substring filter for category")
		logLevelStr          = flag.String("log-level", "info", "slog level (debug, info, warn, error)")
		showVersion          = flag.Bool("version", false, "Print version")
		noPgRegress          = flag.Bool("no-pg-regress", false, "Skip upstream pg_regress suite (default: run it; requires Docker and `make upstream-N`)")
		noPgIsolationRegress = flag.Bool("no-isolation-regress", false, "Skip upstream pg_isolation_regress suite (default: run it; requires Docker and `make upstream-N`)")
		pgVersion            = flag.Int("pg-version", 18, "PG major to assert compatibility against (14, 15, 16, 17, or 18). Used to select which upstream corpus and postgres:N image the regression suites run with — independent of what the target reports.")
		allowDirty           = flag.Bool("allow-dirty", false, "Skip the fresh-DB pre-flight check. pg_regress requires a fresh DB; results are unreliable on a populated target.")
		dockerContainer      = flag.String("docker-container", "", "Name of the target's Docker container. When set, the harness runs `docker restart <name>` between regressions and YAML probes so pg_stat_statements (the only contrib extension whose SQL surface requires shared_preload_libraries) can be loaded. If unset, the pg_stat_statements probe fails with PG error 'object not in prerequisite state' (extension not preloaded) — same as a managed-service target.")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("postgres-compat", version)
		return 0
	}

	// Logging
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevelStr)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid log level: %v\n", err)
		return 1
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if *urlStr == "" {
		slog.Error("missing database URL (set -url or PGURL)")
		return 1
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runStart := time.Now()

	// 1. Pre-flight checks and setup
	runID = generateSchemaID()
	slog.Debug("run-level schema generated", "schema", runID)

	poolConfig, err := pgxpool.ParseConfig(*urlStr)
	if err != nil {
		slog.Error("failed to parse connection URL", "err", err)
		return 1
	}
	poolConfig.MaxConns = int32(*parallel)
	// Apply search_path to all connections
	poolConfig.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, fmt.Sprintf("SET search_path TO %s, public", runID))
		return err
	}

	targetDB = poolConfig.ConnConfig.Database
	targetUser = poolConfig.ConnConfig.User

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		slog.Error("failed to create connection pool", "err", err)
		return 1
	}
	defer pool.Close()

	if *primaryURL != "" {
		primaryConfig, err := pgxpool.ParseConfig(*primaryURL)
		if err != nil {
			slog.Error("failed to parse -primary-url", "err", err)
			return 1
		}
		primaryConfig.MaxConns = int32(*parallel)
		primaryConfig.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, fmt.Sprintf("SET search_path TO %s, public", runID))
			return err
		}
		primaryPool, err = pgxpool.NewWithConfig(ctx, primaryConfig)
		if err != nil {
			slog.Error("failed to create primary connection pool", "err", err)
			return 1
		}
		defer primaryPool.Close()
	}

	// Pre-flight: pg_regress requires a fresh DB. If the target already has user
	// objects in non-system schemas, abort early — running anyway produces
	// spurious diffs in fast_default/select_parallel/vacuum_parallel from
	// leftover state. -allow-dirty skips this for users who know what they're doing.
	if !*allowDirty {
		if dirty, err := checkFreshDB(ctx, pool); err != nil {
			slog.Error("fresh-DB pre-flight check failed", "err", err)
			return 1
		} else if dirty != "" {
			slog.Error("target DB is not fresh — pg_regress requires a clean database",
				"detail", dirty,
				"hint", "wrap your run in: createdb pgcompat_run && PGURL=…/pgcompat_run go run . && dropdb pgcompat_run; or pass -allow-dirty")
			return 1
		}
	}

	// Query Server Version (purely informational; recorded in the report).
	var srvVersion string
	var srvVersionNum int
	var srvMajor int
	err = pool.QueryRow(ctx, "SELECT current_setting('server_version'), current_setting('server_version_num')::int, current_setting('server_version_num')::int / 10000").Scan(&srvVersion, &srvVersionNum, &srvMajor)
	if err != nil {
		slog.Error("failed to query server version", "err", err)
		return 1
	}
	slog.Info("connected to target", "version", srvVersion, "major", srvMajor)

	// Detect read-only targets: hot standby (pg_is_in_recovery) or session-level
	// read-only enforcement (default_transaction_read_only = on).
	if err := pool.QueryRow(ctx,
		`SELECT pg_is_in_recovery() OR current_setting('default_transaction_read_only')::bool`,
	).Scan(&readOnlyTarget); err != nil {
		slog.Error("failed to detect read-only state", "err", err)
		return 1
	}
	var primaryVersion string
	if primaryPool != nil {
		if err := primaryPool.QueryRow(ctx, "SELECT current_setting('server_version')").Scan(&primaryVersion); err != nil {
			slog.Error("failed to query primary server version", "err", err)
			return 1
		}
		if readOnlyTarget {
			slog.Info("target is read-only — alter:true tests will run against primary", "primary_version", primaryVersion)
		} else {
			slog.Info("primary pool configured — alter:true tests will run against primary", "primary_version", primaryVersion)
		}
	} else if readOnlyTarget {
		slog.Info("target is read-only — alter:true tests will be skipped (pass -primary-url to run them against a primary)")
	}

	var results []Result

	// Validate -pg-version against the supported set if either regress suite
	// will run. corpusFor is the source of truth for which majors we ship.
	if !*noPgRegress || !*noPgIsolationRegress {
		if _, ok := corpusFor(*pgVersion); !ok {
			slog.Error("unsupported -pg-version (must be 14, 15, 16, 17, or 18)", "got", *pgVersion)
			return 1
		}
	}

	// Run upstream regression suites FIRST, against a target that has only
	// the system catalogs and the user's pre-existing state. CREATE EXTENSION
	// of our test extensions, our run-level schema, and any other harness-
	// induced state happen *after* this — pg_regress and pg_isolation_regress
	// expected outputs are sensitive to extra catalog entries (extensions add
	// operators, types, namespaces) and to non-default search_path.
	if !*noPgRegress {
		cfg := poolConfig.ConnConfig
		slog.Info("running pg_regress suite", "host", cfg.Host, "port", cfg.Port, "db", cfg.Database, "pg_version", *pgVersion)
		for _, r := range runPgRegress(ctx, *urlStr, *pgVersion) {
			if *coreOnly && !r.Test.Core {
				continue
			}
			if !matchesFilter(r.Test.Domain, r.Test.Topic, *category) {
				continue
			}
			slog.Debug("regress", "id", r.Test.ID, "passed", r.Passed(), "ms", r.DurationMS)
			results = append(results, r)
		}
	}

	if !*noPgIsolationRegress {
		cfg := poolConfig.ConnConfig
		slog.Info("running pg_isolation_regress suite", "host", cfg.Host, "port", cfg.Port, "pg_version", *pgVersion)
		for _, r := range runPgIsolationRegress(ctx, *urlStr, *pgVersion) {
			if *coreOnly && !r.Test.Core {
				continue
			}
			if !matchesFilter(r.Test.Domain, r.Test.Topic, *category) {
				continue
			}
			slog.Debug("isolation-regress", "id", r.Test.ID, "passed", r.Passed(), "ms", r.DurationMS)
			results = append(results, r)
		}
	}

	// Some core/contrib extensions (pg_stat_statements, auto_explain, pg_prewarm)
	// need shared_preload_libraries set + a server restart before CREATE EXTENSION
	// is meaningful. If our test suite references any of them and -restart-cmd is
	// provided, merge them into shared_preload_libraries, run the restart command,
	// wait for the target to come back, and recreate the pool. Without -restart-cmd,
	// those tests will fail with SQLSTATE 55000 — same as today.
	preloadNeeded := extensionsRequiringPreload(Tests)
	if len(preloadNeeded) > 0 {
		if *dockerContainer == "" {
			slog.Info("tests reference extensions that need shared_preload_libraries — pass -docker-container to enable, otherwise they will fail (extension not preloaded)",
				"extensions", preloadNeeded)
		} else {
			var existing string
			if err := pool.QueryRow(ctx, "SHOW shared_preload_libraries").Scan(&existing); err != nil {
				slog.Error("failed to read shared_preload_libraries", "err", err)
				return 1
			}
			merged := mergePreloadList(existing, preloadNeeded)
			if merged != strings.TrimSpace(existing) {
				if _, err := pool.Exec(ctx, "ALTER SYSTEM SET shared_preload_libraries = '"+strings.ReplaceAll(merged, "'", "''")+"'"); err != nil {
					slog.Error("failed to ALTER SYSTEM SET shared_preload_libraries", "err", err)
					return 1
				}
				slog.Info("restarting target docker container", "container", *dockerContainer, "shared_preload_libraries", merged)
				restart := exec.CommandContext(ctx, "docker", "restart", *dockerContainer)
				restart.Stdout = os.Stderr
				restart.Stderr = os.Stderr
				if err := restart.Run(); err != nil {
					slog.Error("docker restart failed", "container", *dockerContainer, "err", err)
					return 1
				}
				pool.Close()
				if err := waitForReady(ctx, *urlStr, 60*time.Second); err != nil {
					slog.Error("target did not come back after restart", "err", err)
					return 1
				}
				pool, err = pgxpool.NewWithConfig(ctx, poolConfig)
				if err != nil {
					slog.Error("failed to recreate pool after restart", "err", err)
					return 1
				}
				defer pool.Close()
			}
		}
	}

	// Now (and only now) set up our test environment. Regressions have completed;
	// we can install our extensions and run-level schema without perturbing them.
	// On a read-only replica without a primary pool the schema cannot be created;
	// alter:true tests are skipped. When a primary pool is available, schema and
	// extension setup runs there — writes replicate to the replica automatically.
	writePool := pool
	if primaryPool != nil {
		writePool = primaryPool
	}
	if !readOnlyTarget || primaryPool != nil {
		_, err = writePool.Exec(context.Background(), fmt.Sprintf("CREATE SCHEMA %s", runID))
		if err != nil {
			if sqlStateOf(err) == "25006" {
				// Proxy or middleware enforces read-only at the network layer —
				// not caught by our GUC probes above. Treat as read-only.
				readOnlyTarget = true
				slog.Info("target is read-only (detected from CREATE SCHEMA) — alter:true tests will be skipped")
			} else {
				slog.Error("failed to create run schema", "err", err)
				return 1
			}
		} else {
			defer func() {
				slog.Debug("cleaning up run schema", "schema", runID)
				_, dropErr := writePool.Exec(context.Background(), fmt.Sprintf("DROP SCHEMA %s CASCADE", runID))
				if dropErr != nil {
					slog.Warn("failed to drop run schema", "schema", runID, "err", dropErr)
				}
			}()
		}
	}

	// Extension gate: collect every distinct extension declared by tests, attempt
	// CREATE EXTENSION for each, and record which succeeded. Tests that declare
	// an unavailable extension are Skipped (not failed) downstream. Extensions
	// we installed fresh are dropped at run end (CASCADE; safe because nothing
	// pre-existing depends on them). On a read-only target, provisioning runs
	// against the primary pool so extensions replicate to the replica.
	var freshExtensions []string
	availableExtensions, freshExtensions = provisionExtensions(ctx, writePool, Tests)
	defer func() {
		for _, ext := range freshExtensions {
			if _, err := writePool.Exec(context.Background(), fmt.Sprintf("DROP EXTENSION IF EXISTS %q CASCADE", ext)); err != nil {
				slog.Warn("failed to drop extension", "ext", ext, "err", err)
			}
		}
	}()

	var activeTests []TestCase
	for _, tc := range Tests {
		if (*coreOnly && !tc.Core) || !matchesFilter(tc.Domain, tc.Topic, *category) {
			continue
		}
		activeTests = append(activeTests, tc)
	}

	// Execution primitives
	sem := make(chan struct{}, *parallel)
	var serialMu sync.RWMutex
	var resMu sync.Mutex

	var wg sync.WaitGroup

	for _, tc := range activeTests {
		wg.Add(1)
		go func(tc TestCase) {
			defer wg.Done()

			// Concurrency control
			if tc.Serial {
				serialMu.Lock()
				defer serialMu.Unlock()
			} else {
				sem <- struct{}{}
				serialMu.RLock()
				defer serialMu.RUnlock()
				defer func() { <-sem }()
			}

			res := runTest(ctx, pool, tc, srvMajor, *timeout)
			slog.Debug("test", "id", res.Test.ID, "passed", res.Passed(), "ms", res.DurationMS, "silent_failure", res.SilentFailure)

			resMu.Lock()
			results = append(results, res)
			resMu.Unlock()

		}(tc)
	}

	// Wait for YAML tests to drain
	wg.Wait()

	runDuration := time.Since(runStart)

	// Compile Report
	rep := Report{
		SchemaVersion: 2,
		GeneratedAt:   time.Now().UTC(),
	}
	rep.Target.ServerVersion = srvVersion
	rep.Target.ServerVersionNum = srvVersionNum
	rep.Target.ServerMajor = srvMajor
	rep.Target.PrimaryServerVersion = primaryVersion
	rep.Run.Parallel = *parallel
	rep.Run.DurationMS = runDuration.Milliseconds()

	var anyCoreFailed bool

	domainMap := map[string]map[string][]TestOutput{}

	for _, r := range results {
		if r.Skipped {
			rep.Summary.Skipped++
			continue
		}

		passed := r.Passed()
		out := TestOutput{
			ID:            r.Test.ID,
			Core:          r.Test.Core,
			Alter:         r.Test.Alter,
			Passed:        passed,
			DurationMS:    r.DurationMS,
			SilentFailure: r.SilentFailure,
			SQLState:      r.SQLState,
		}
		if r.Err != nil {
			out.Reason = r.Err.Error()
		}

		if r.SilentFailure {
			rep.Summary.SilentFailures++
		}

		if r.Test.Core {
			rep.Summary.CoreTotal++
			if passed {
				rep.Summary.CorePassed++
			} else {
				anyCoreFailed = true
			}
		} else {
			rep.Summary.OptionalTotal++
			if passed {
				rep.Summary.OptionalPassed++
			}
		}

		if domainMap[r.Test.Domain] == nil {
			domainMap[r.Test.Domain] = map[string][]TestOutput{}
		}
		domainMap[r.Test.Domain][r.Test.Topic] = append(domainMap[r.Test.Domain][r.Test.Topic], out)
	}

	rep.Domains = compileDomains(domainMap)

	// Write YAML
	yamlBytes, err := yaml.Marshal(&rep)
	if err != nil {
		slog.Error("failed to marshal YAML report", "err", err)
		return 1
	}
	if err := os.WriteFile(*outPath, yamlBytes, 0644); err != nil {
		slog.Error("failed to write output file", "path", *outPath, "err", err)
		return 1
	}

	// Single summary log
	slog.Info("run complete",
		"core", fmt.Sprintf("%d/%d", rep.Summary.CorePassed, rep.Summary.CoreTotal),
		"optional", fmt.Sprintf("%d/%d", rep.Summary.OptionalPassed, rep.Summary.OptionalTotal),
		"silent_failures", rep.Summary.SilentFailures,
		"skipped", rep.Summary.Skipped,
		"duration", runDuration.Round(time.Millisecond))

	if anyCoreFailed {
		return 1
	}
	return 0
}

// compileDomains assembles the report's domain/topic tree. Known domains and
// topics appear in the order recorded by the loader's directory traversal
// (DomainOrder, TopicOrder).  Extra domains/topics produced by pg_regress are
// appended at the end in alphabetical order.
func compileDomains(domainMap map[string]map[string][]TestOutput) []DomainResult {
	out := make([]DomainResult, 0, len(domainMap))
	seenDomains := map[string]bool{}

	for _, domain := range DomainOrder {
		topicMap, ok := domainMap[domain]
		if !ok || len(topicMap) == 0 {
			continue
		}
		seenDomains[domain] = true
		seenTopics := map[string]bool{}
		var topics []TopicResult
		for _, topic := range TopicOrder[domain] {
			tests := topicMap[topic]
			if len(tests) == 0 {
				continue
			}
			seenTopics[topic] = true
			sort.Slice(tests, func(i, j int) bool { return tests[i].ID < tests[j].ID })
			topics = append(topics, TopicResult{ID: topic, Tests: tests})
		}
		// Extra topics from pg_regress not in TopicOrder (alphabetical)
		var extra []string
		for t := range topicMap {
			if !seenTopics[t] {
				extra = append(extra, t)
			}
		}
		sort.Strings(extra)
		for _, t := range extra {
			tests := topicMap[t]
			sort.Slice(tests, func(i, j int) bool { return tests[i].ID < tests[j].ID })
			topics = append(topics, TopicResult{ID: t, Tests: tests})
		}
		if len(topics) > 0 {
			out = append(out, DomainResult{ID: domain, Topics: topics})
		}
	}

	// Extra domains from pg_regress not in DomainOrder (alphabetical)
	var extraDomains []string
	for d := range domainMap {
		if !seenDomains[d] {
			extraDomains = append(extraDomains, d)
		}
	}
	sort.Strings(extraDomains)
	for _, domain := range extraDomains {
		topicMap := domainMap[domain]
		var topicNames []string
		for t := range topicMap {
			topicNames = append(topicNames, t)
		}
		sort.Strings(topicNames)
		var topics []TopicResult
		for _, t := range topicNames {
			tests := topicMap[t]
			sort.Slice(tests, func(i, j int) bool { return tests[i].ID < tests[j].ID })
			topics = append(topics, TopicResult{ID: t, Tests: tests})
		}
		if len(topics) > 0 {
			out = append(out, DomainResult{ID: domain, Topics: topics})
		}
	}
	return out
}

func runTest(ctx context.Context, pool *pgxpool.Pool, tc TestCase, srvMajor int, timeout time.Duration) (res Result) {
	start := time.Now()
	res.Test = tc
	defer func() {
		if r := recover(); r != nil {
			res.Err = fmt.Errorf("panic: %v", r)
		}
		res.SQLState = sqlStateOf(res.Err)
		res.DurationMS = time.Since(start).Milliseconds()
	}()

	switch {
	case !tc.AppliesTo(srvMajor):
		res.Skipped = true
		res.SkipReason = fmt.Sprintf("not declared for PG %d", srvMajor)
		return
	case tc.Extension != "" && !availableExtensions[tc.Extension]:
		res.Skipped = true
		res.SkipReason = fmt.Sprintf("extension %q not available", tc.Extension)
		return
	case readOnlyTarget && tc.Alter && primaryPool == nil:
		res.Skipped = true
		res.SkipReason = "target is a read-only replica"
		return
	}

	// Route alter:true tests to the primary pool when available.
	activePool := pool
	if tc.Alter && primaryPool != nil {
		activePool = primaryPool
	}

	conn, err := activePool.Acquire(ctx)
	if err != nil {
		res.Err = fmt.Errorf("failed to acquire connection: %w", err)
		return
	}
	defer releaseConn(conn)

	tCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for _, sql := range tc.Setup {
		if _, err := conn.Exec(tCtx, expandSQL(sql, runID)); err != nil {
			res.Err = fmt.Errorf("setup failed: %w", err)
			return
		}
	}
	defer func() {
		for _, sql := range tc.Teardown {
			conn.Exec(context.Background(), expandSQL(sql, runID))
		}
	}()

	// Probe — every statement except the last must succeed; ExpectError
	// applies only to the final statement.
	for i, sql := range tc.Probe {
		_, err = conn.Exec(tCtx, expandSQL(sql, runID))
		if i < len(tc.Probe)-1 {
			if err != nil {
				res.Err = fmt.Errorf("probe pre-statement failed: %w", err)
				return
			}
			continue
		}
		switch {
		case tc.ExpectError == "" && err != nil:
			res.Err = fmt.Errorf("probe failed: %w", err)
			return
		case tc.ExpectError != "" && err == nil:
			res.Err = fmt.Errorf("expected SQLSTATE %s, got success", tc.ExpectError)
			return
		case tc.ExpectError != "" && sqlStateOf(err) != tc.ExpectError:
			res.Err = fmt.Errorf("expected SQLSTATE %s, got %v: %w", tc.ExpectError, sqlStateOf(err), err)
			return
		}
		err = nil
	}

	if tc.Validate != nil {
		if valErr := tc.Validate(tCtx, conn.Conn()); valErr != nil {
			res.SilentFailure = true
			res.Err = fmt.Errorf("silent failure: %w", valErr)
		}
	}
	return
}

// releaseConn returns conn to the pool, rolling back any open transaction and
// clearing GUCs so the next test on it starts clean. Dead connections are
// hijacked and closed.
func releaseConn(conn *pgxpool.Conn) {
	if status := conn.Conn().PgConn().TxStatus(); status == 'T' || status == 'E' {
		if _, err := conn.Exec(context.Background(), "ROLLBACK"); err != nil {
			conn.Hijack()
			conn.Conn().Close(context.Background())
			return
		}
	}
	conn.Exec(context.Background(), fmt.Sprintf("RESET ALL; SET search_path TO %s, public", runID))
	conn.Release()
}

// expandSQL replaces YAML SQL placeholders with their runtime values:
//   - {runid}  → per-run schema ID (so database-global objects like roles
//     are unique per run)
//   - {dbname} → the target database name parsed from the connection URL
//   - {user}   → the connecting role name from the connection URL (used by
//     postgres_fdw self-loopback so the FDW connection authenticates as the
//     connecting role rather than libpq's default OS user)
func expandSQL(sql, schemaID string) string {
	s := strings.ReplaceAll(sql, "{runid}", schemaID)
	s = strings.ReplaceAll(s, "{dbname}", targetDB)
	s = strings.ReplaceAll(s, "{user}", targetUser)
	return s
}

// provisionExtensions inspects every test's Extension field, attempts CREATE
// EXTENSION IF NOT EXISTS for each distinct name, and returns the set of
// extensions usable by tests plus the subset we installed fresh (caller drops
// these at run end). CREATE EXTENSION failure is non-fatal — the extension is
// simply marked unavailable, and dependent tests get Skipped downstream.
//
// On a read-only target we skip the CREATE EXTENSION attempt entirely and only
// mark extensions that are already installed as available.
func provisionExtensions(ctx context.Context, pool *pgxpool.Pool, tests []TestCase) (available map[string]bool, fresh []string) {
	available = map[string]bool{}
	for _, tc := range tests {
		ext := tc.Extension
		if ext == "" || available[ext] {
			continue
		}
		var existed bool
		if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname = $1)", ext).Scan(&existed); err != nil {
			slog.Warn("failed to query pg_extension", "ext", ext, "err", err)
			continue
		}
		if !existed {
			if _, err := pool.Exec(ctx, fmt.Sprintf("CREATE EXTENSION %q", ext)); err != nil {
				if readOnlyTarget && primaryPool == nil {
					// No write path available — expected, log at debug.
					slog.Debug("read-only target — extension not available, dependent tests will be skipped", "ext", ext)
				} else {
					slog.Info("extension not available — dependent tests will be skipped", "ext", ext, "err", err)
				}
				continue
			}
			fresh = append(fresh, ext)
		}
		available[ext] = true
	}
	return available, fresh
}

// matchesFilter returns true if needle is empty or matches (case-insensitive substring)
// either the test's domain or its topic. Used to honour the -category CLI flag.
func matchesFilter(domain, topic, needle string) bool {
	if needle == "" {
		return true
	}
	n := strings.ToLower(needle)
	return strings.Contains(strings.ToLower(domain), n) ||
		strings.Contains(strings.ToLower(topic), n)
}

// requiresPreload lists core/contrib extensions whose user-facing SQL surface
// genuinely refuses to work without shared_preload_libraries. Currently just
// pg_stat_statements: the SRF C functions emit ERRCODE_OBJECT_NOT_IN_PREREQUISITE_STATE
// (55000) at execution time if the library isn't loaded.
//
// auto_explain is intentionally NOT here: LOAD 'auto_explain' in a session
// works without a server restart. pg_prewarm is also NOT here: the pg_prewarm()
// SRF works without preload — only the autoprewarm worker needs it.
//
// Third-party extensions (pgaudit, pg_cron, etc.) are out of scope — this
// harness tests core PostgreSQL + contrib only.
var requiresPreload = map[string]bool{
	"pg_stat_statements": true,
}

// extensionsRequiringPreload returns the sorted distinct list of preload-
// requiring extensions referenced by the given tests.
func extensionsRequiringPreload(tests []TestCase) []string {
	seen := map[string]bool{}
	var out []string
	for _, tc := range tests {
		if tc.Extension != "" && requiresPreload[tc.Extension] && !seen[tc.Extension] {
			seen[tc.Extension] = true
			out = append(out, tc.Extension)
		}
	}
	sort.Strings(out)
	return out
}

// mergePreloadList unions an existing comma-separated shared_preload_libraries
// value with new entries, preserving order and removing duplicates and blanks.
func mergePreloadList(existing string, additions []string) string {
	seen := map[string]bool{}
	var ordered []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		ordered = append(ordered, s)
	}
	for _, s := range strings.Split(existing, ",") {
		add(s)
	}
	for _, s := range additions {
		add(s)
	}
	return strings.Join(ordered, ",")
}

// waitForReady polls the target with SELECT 1 until it succeeds or the timeout
// elapses. Used after a restart to wait for the target to come back up.
func waitForReady(ctx context.Context, urlStr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		conn, err := pgx.Connect(cctx, urlStr)
		cancel()
		if err == nil {
			cctx2, cancel2 := context.WithTimeout(ctx, 2*time.Second)
			var ok int
			err = conn.QueryRow(cctx2, "SELECT 1").Scan(&ok)
			cancel2()
			conn.Close(context.Background())
			if err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("timeout after %s waiting for target to become ready", timeout)
}

// checkFreshDB returns a non-empty description string if the target DB has
// user-owned relations in non-system schemas — i.e. it isn't fresh. Empty
// string means the DB is fresh and pg_regress can run safely. pgcompat_*
// schemas (leftover from killed prior runs) are tolerated since we drop them.
func checkFreshDB(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	const q = `
		SELECT n.nspname || '.' || c.relname
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')
		  AND n.nspname NOT LIKE 'pg_%'
		  AND n.nspname NOT LIKE 'pgcompat_%'
		  AND c.relkind IN ('r', 'm', 'v', 'S', 'f', 'p')
		ORDER BY 1
		LIMIT 5`
	rows, err := pool.Query(ctx, q)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var found []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", err
		}
		found = append(found, name)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "", nil
	}
	return fmt.Sprintf("found %s (showing up to 5)", strings.Join(found, ", ")), nil
}

func generateSchemaID() string {
	b := make([]byte, 2)
	rand.Read(b)
	return fmt.Sprintf("pgcompat_%d_%s", time.Now().Unix(), hex.EncodeToString(b))
}
