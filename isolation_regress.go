package main

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
)

// runPgIsolationRegress runs the upstream isolation schedule inside the
// postgres:<pgMajor> Docker container pointed at the target.  Requires Docker
// on PATH and the upstream isolation corpus at
// upstream/postgres-<pgMajor>/src/test/isolation.  Returns nil if either is
// absent or the major isn't supported.
func runPgIsolationRegress(ctx context.Context, urlStr string, pgMajor int) []Result {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil
	}
	corpus, ok := corpusFor(pgMajor)
	if !ok {
		slog.Warn("unsupported -pg-version for pg_isolation_regress, skipping", "pg_version", pgMajor)
		return nil
	}
	absInput, err := filepath.Abs(corpus.isolationSrcDir)
	if err != nil || !dirExists(absInput) {
		slog.Warn("upstream isolation corpus missing — run `make upstream-N`",
			"pg_version", pgMajor, "expected_path", corpus.isolationSrcDir)
		return nil
	}

	tmpInput, err := os.MkdirTemp("", "pgcompat-isolation-in-*")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(tmpInput)
	if err := exec.Command("cp", "-a", absInput+"/.", tmpInput+"/").Run(); err != nil {
		return nil
	}

	out, err := runDockeredEngine(ctx, corpus.image, corpus.isolationBin,
		"isolation", tmpInput, urlStr, []string{
			"--schedule=/isolation/isolation_schedule",
		})
	if err != nil {
		return nil
	}
	return parseTAPOutput(out, "isolation-regress-", "upstream isolation: ",
		func(string) (string, string, bool) {
			return "behavior_and_core_experience", "transaction_isolation", true
		})
}
