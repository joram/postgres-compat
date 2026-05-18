package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// pgCorpus describes the per-major upstream test corpus and the matching
// docker image / binary paths used to run it.
type pgCorpus struct {
	image, regressBin, isolationBin, srcDir, isolationSrcDir string
}

// corpusFor returns the corpus descriptor for a supported PG major version.
// Both the postgres:N image layout and the upstream/postgres-N/ on-disk layout
// follow a stable per-major pattern, so we derive everything from the integer.
func corpusFor(major int) (pgCorpus, bool) {
	switch major {
	case 14, 15, 16, 17, 18:
		n := strconv.Itoa(major)
		return pgCorpus{
			image:           "postgres:" + n,
			regressBin:      "/usr/lib/postgresql/" + n + "/lib/pgxs/src/test/regress/pg_regress",
			isolationBin:    "/usr/lib/postgresql/" + n + "/lib/pgxs/src/test/isolation/pg_isolation_regress",
			srcDir:          "upstream/postgres-" + n + "/src/test/regress",
			isolationSrcDir: "upstream/postgres-" + n + "/src/test/isolation",
		}, true
	}
	return pgCorpus{}, false
}

// regressOutOfScope lists upstream tests that exercise PostgreSQL's own source
// invariants or developer tooling rather than user-facing SQL surface, so they
// don't belong in a vendor compatibility harness. They're stripped from the
// schedule we hand pg_regress; they never run.
//
//   - test_setup            replicated by preloadRegressFixtures (libpq only)
//   - opr_sanity, type_sanity, misc_sanity, sanity_check, oidjoins
//     catalog/operator/type sanity checks for PG itself
//   - create_function_c     tests the C-extension loading mechanism
//   - psql, psql_crosstab   test the psql client, not the server
var regressOutOfScope = map[string]bool{
	"test_setup":        true,
	"opr_sanity":        true,
	"type_sanity":       true,
	"misc_sanity":       true,
	"sanity_check":      true,
	"oidjoins":          true,
	"create_function_c": true,
	"psql":              true,
	"psql_crosstab":     true,
}

// tapResultRE matches one TAP-format line emitted by pg_regress and
// pg_isolation_regress: `(ok|not ok) N - <name> <ms> ms`.
var tapResultRE = regexp.MustCompile(`^(ok|not ok)\s+\d+\s+-\s+(\S+)\s+(\d+) ms`)

// runPgRegress runs the upstream pg_regress parallel_schedule inside the
// postgres:<pgMajor> Docker container pointed at the target.  Requires Docker
// on PATH and the upstream regress corpus at upstream/postgres-<pgMajor>/.
//
// Strategy:
//  1. Replicate the test_setup.sql preload over libpq: inline DDL/DML via
//     pgx.Conn.Exec, COPY blocks via the standard COPY FROM STDIN protocol
//     (pgconn.CopyFrom).  No psql, no docker, no in-memory rewrite.
//  2. Run pg_regress against a filtered schedule that omits the out-of-scope
//     tests (see regressOutOfScope).  Upstream sql/ and expected/ files are
//     not modified; only the schedule (which is ours to compose) is filtered.
func runPgRegress(ctx context.Context, urlStr string, pgMajor int) []Result {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	corpus, ok := corpusFor(pgMajor)
	if !ok {
		slog.Warn("unsupported -pg-version for pg_regress, skipping", "pg_version", pgMajor)
		return nil
	}
	absInput, err := filepath.Abs(corpus.srcDir)
	if err != nil || !dirExists(absInput) {
		slog.Warn("upstream regress corpus missing — run `make upstream-N`",
			"pg_version", pgMajor, "expected_path", corpus.srcDir)
		return nil
	}

	tmpInput, cleanup, err := buildRegressInput(absInput)
	if err != nil {
		return nil
	}
	defer cleanup()

	if err := preloadRegressFixtures(ctx, urlStr, filepath.Join(absInput, "data"), filepath.Join(absInput, "sql", "test_setup.sql")); err != nil {
		slog.Warn("regress fixture preload reported errors (continuing)", "err", err)
	}

	_, _, _, _, dbname, err := parseConnURL(urlStr)
	if err != nil {
		slog.Warn("could not parse connection URL for pg_regress", "err", err)
		return nil
	}

	out, err := runDockeredEngine(ctx, corpus.image, corpus.regressBin,
		"regress", tmpInput, urlStr, []string{
			"--dbname=" + dbname,
			"--use-existing",
			"--schedule=/regress/pgcompat_schedule",
		})
	if err != nil {
		return nil
	}
	return parseTAPOutput(out, "regress-", "upstream regression: ", regressTopicFor)
}

// runDockeredEngine invokes a pg_regress-shaped binary inside a postgres:N
// container against the target. mountSrc is mounted read-only at /<mountName>;
// a tmp output dir is mounted at /out and chmod 777'd by busybox before
// removal so files written as the in-container postgres user are removable.
// The binary always receives --host/--port/--user/--inputdir/--outputdir;
// extraArgs is appended for engine-specific flags (--use-existing, --schedule).
func runDockeredEngine(ctx context.Context, image, binary, mountName, mountSrc, urlStr string, extraArgs []string) ([]byte, error) {
	host, port, user, password, _, err := parseConnURL(urlStr)
	if err != nil {
		return nil, err
	}
	tmpOut, err := os.MkdirTemp("", "pgcompat-out-*")
	if err != nil {
		return nil, err
	}
	defer func() {
		exec.Command("docker", "run", "--rm",
			"-v", tmpOut+":/out",
			"busybox", "chmod", "-R", "777", "/out").Run()
		os.RemoveAll(tmpOut)
	}()

	args := []string{
		"run", "--rm",
		"--network=host",
		"-v", mountSrc + ":/" + mountName + ":ro",
		"-v", tmpOut + ":/out",
		"-e", "PGPASSWORD=" + password,
		image,
		binary,
		"--host", host, "--port", port, "--user", user,
		"--inputdir=/" + mountName,
		"--outputdir=/out",
	}
	args = append(args, extraArgs...)

	out, _ := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return out, nil
}

// parseTAPOutput parses pg_regress / pg_isolation_regress TAP output into
// Result records. idPrefix is prepended to the test name for the report ID;
// classify maps a test name to its (domain, topic, core) classification.
func parseTAPOutput(out []byte, idPrefix, descPrefix string, classify func(string) (string, string, bool)) []Result {
	var results []Result
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		m := tapResultRE.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		status, name, msStr := m[1], m[2], m[3]
		ms, _ := strconv.ParseInt(msStr, 10, 64)
		domain, topic, core := classify(name)
		tc := TestCase{
			ID:          idPrefix + name,
			Domain:      domain,
			Topic:       topic,
			Core:        core,
			Description: descPrefix + name,
		}
		r := Result{Test: tc, DurationMS: ms}
		if status == "not ok" {
			r.Err = fmt.Errorf("regression test failed")
		}
		results = append(results, r)
	}
	return results
}

// buildRegressInput creates a temp dir containing a verbatim copy of the
// upstream regress source plus a filtered schedule (`pgcompat_schedule`) with
// the out-of-scope test lines stripped.  Upstream sql/ and expected/ are not
// modified.
func buildRegressInput(src string) (string, func(), error) {
	tmp, err := os.MkdirTemp("", "pgcompat-regress-in-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }

	if err := exec.Command("cp", "-a", src+"/.", tmp+"/").Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("cp regress dir: %w", err)
	}

	srcSched, err := os.ReadFile(filepath.Join(tmp, "parallel_schedule"))
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("read parallel_schedule: %w", err)
	}
	filtered := filterSchedule(srcSched, regressOutOfScope)
	if err := os.WriteFile(filepath.Join(tmp, "pgcompat_schedule"), filtered, 0644); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write pgcompat_schedule: %w", err)
	}
	return tmp, cleanup, nil
}

// filterSchedule rewrites a pg_regress schedule, dropping any test names listed
// in `excluded` from `test:` lines.  A line that ends up with no remaining
// tests is omitted entirely.  Comment and blank lines pass through unchanged.
func filterSchedule(src []byte, excluded map[string]bool) []byte {
	var out bytes.Buffer
	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "test:") {
			out.WriteString(line)
			out.WriteByte('\n')
			continue
		}
		var kept []string
		for _, name := range strings.Fields(strings.TrimPrefix(trim, "test:")) {
			if !excluded[name] {
				kept = append(kept, name)
			}
		}
		if len(kept) == 0 {
			continue
		}
		out.WriteString("test: ")
		out.WriteString(strings.Join(kept, " "))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// setFilenameRE captures the data path from `\set filename :abs_srcdir '/data/X.data'`.
var setFilenameRE = regexp.MustCompile(`(?i)^\s*\\set\s+filename\s+:abs_srcdir\s+'([^']+)'`)

// copyFromVarRE matches `COPY <tbl> FROM :'filename';` (whitespace-tolerant).
var copyFromVarRE = regexp.MustCompile(`(?i)^\s*COPY\s+(\S+)\s+FROM\s+:'filename'\s*;\s*$`)

// preloadRegressFixtures replicates upstream's test_setup.sql preload over
// libpq.  Inline DDL/DML runs via pgx.Conn.Exec; `COPY t FROM :'filename'`
// streams the local data file via the standard COPY FROM STDIN protocol.
// Statement errors (CREATE TABLESPACE without allow_in_place_tablespaces,
// CREATE FUNCTION ... LANGUAGE C without regress.so) are logged at debug and
// skipped — the downstream tests we keep don't depend on them.
func preloadRegressFixtures(ctx context.Context, urlStr, dataDir, setupPath string) error {
	src, err := os.ReadFile(setupPath)
	if err != nil {
		return fmt.Errorf("read test_setup.sql: %w", err)
	}
	conn, err := pgx.Connect(ctx, urlStr)
	if err != nil {
		return fmt.Errorf("preload connect: %w", err)
	}
	defer conn.Close(ctx)

	var stmt []byte
	var dataFile string
	flush := func() {
		if len(bytes.TrimSpace(stmt)) > 0 {
			if _, err := conn.Exec(ctx, string(stmt)); err != nil {
				slog.Debug("preload statement error (continuing)", "err", err)
			}
		}
		stmt = stmt[:0]
	}

	scanner := bufio.NewScanner(bytes.NewReader(src))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		trim := bytes.TrimLeft(line, " \t")

		if bytes.HasPrefix(trim, []byte("\\")) {
			if m := setFilenameRE.FindSubmatch(line); m != nil {
				dataFile = filepath.Join(dataDir, filepath.Base(string(m[1])))
			}
			continue
		}
		if m := copyFromVarRE.FindSubmatch(line); m != nil && dataFile != "" {
			flush()
			tbl := string(m[1])
			f, openErr := os.Open(dataFile)
			if openErr != nil {
				return fmt.Errorf("open %s: %w", dataFile, openErr)
			}
			_, copyErr := conn.PgConn().CopyFrom(ctx, f, "COPY "+tbl+" FROM STDIN")
			f.Close()
			if copyErr != nil {
				slog.Debug("preload copy error (continuing)", "table", tbl, "err", copyErr)
			}
			continue
		}

		stmt = append(stmt, line...)
		stmt = append(stmt, '\n')
		if bytes.HasSuffix(bytes.TrimRight(line, " \t"), []byte(";")) {
			flush()
		}
	}
	flush()
	return scanner.Err()
}

// parseConnURL extracts the host/port/user/password/dbname from a libpq URL,
// for handing off to a docker-run pg_regress invocation.
func parseConnURL(urlStr string) (host, port, user, password, dbname string, err error) {
	cfg, err := pgx.ParseConfig(urlStr)
	if err != nil {
		return "", "", "", "", "", err
	}
	return cfg.Host, strconv.FormatUint(uint64(cfg.Port), 10), cfg.User, cfg.Password, cfg.Database, nil
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
