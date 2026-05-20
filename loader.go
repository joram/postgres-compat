package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"strings"

	"github.com/jackc/pgx/v5"
	"gopkg.in/yaml.v3"
)

//go:embed all:suite
var suiteFS embed.FS

// Tests holds every TestCase loaded from the suite/ tree at startup.
// DomainOrder records the directory traversal order so the report can mirror it.
var (
	Tests       []TestCase
	DomainOrder []string
	TopicOrder  = map[string][]string{} // domain -> ordered topics
)

func init() {
	loadSuite()
}

type yamlTestCase struct {
	ID                string        `yaml:"id"`
	Description       string        `yaml:"description"`
	Core              bool          `yaml:"core"`
	Alter             bool          `yaml:"alter"`
	Serial            bool          `yaml:"serial"`
	SupportedVersions []int         `yaml:"supported_versions"`
	Setup             []string      `yaml:"setup"`
	Probe             []string      `yaml:"probe"`
	ExpectError       string        `yaml:"expect_error"`
	Teardown          []string      `yaml:"teardown"`
	Validate          *yamlValidate `yaml:"validate"`
	Extension         string        `yaml:"extension"`
}

type yamlValidate struct {
	Setup           []string         `yaml:"setup"`
	ReturnsValue    *yamlRetVal      `yaml:"returns_value"`
	ExplainUses     *yamlExplainUses `yaml:"explain_uses"`
	ExplainContains *yamlExplainCont `yaml:"explain_contains"`
}

type yamlRetVal struct {
	SQL    string `yaml:"sql"`
	Equals string `yaml:"equals"`
}

type yamlExplainUses struct {
	SQL       string `yaml:"sql"`
	NodeType  string `yaml:"node_type"`
	IndexName string `yaml:"index_name"`
}

type yamlExplainCont struct {
	SQL      string   `yaml:"sql"`
	Contains []string `yaml:"contains"`
	Excludes []string `yaml:"excludes"`
}

type yamlFile struct {
	Tests []yamlTestCase `yaml:"tests"`
}

// loadSuite walks suite/ and populates Tests, DomainOrder, and TopicOrder.
// Domain identifiers are directory names directly under suite/.
// Topic identifiers are YAML filename stems within each domain directory.
func loadSuite() {
	seenDomain := map[string]bool{}

	err := fs.WalkDir(suiteFS, "suite", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		parts := strings.Split(path, "/")
		if len(parts) != 3 {
			return fmt.Errorf("unexpected suite path %q (want suite/<domain>/<topic>.yaml)", path)
		}
		domain := parts[1]
		topic := strings.TrimSuffix(parts[2], ".yaml")

		if !seenDomain[domain] {
			DomainOrder = append(DomainOrder, domain)
			seenDomain[domain] = true
		}
		TopicOrder[domain] = append(TopicOrder[domain], topic)

		data, err := suiteFS.ReadFile(path)
		if err != nil {
			return err
		}
		var f yamlFile
		if err := yaml.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("failed to parse %s: %w", path, err)
		}
		for _, y := range f.Tests {
			Tests = append(Tests, TestCase{
				ID:                y.ID,
				Domain:            domain,
				Topic:             topic,
				Description:       y.Description,
				Core:              y.Core,
				Alter:             y.Alter,
				Serial:            y.Serial,
				SupportedVersions: y.SupportedVersions,
				Setup:             y.Setup,
				Probe:             y.Probe,
				ExpectError:       y.ExpectError,
				Teardown:          y.Teardown,
				Validate:          buildValidate(y.Validate),
				Extension:         y.Extension,
			})
		}
		return nil
	})
	if err != nil {
		panic(fmt.Sprintf("failed to load test suite: %v", err))
	}
}

// buildValidate converts the optional YAML validate block into a Validate closure.
// Returns nil if v is nil. SQL strings go through expandSQL at call time so {runid} works.
func buildValidate(v *yamlValidate) func(context.Context, *pgx.Conn) error {
	if v == nil {
		return nil
	}
	return func(ctx context.Context, conn *pgx.Conn) error {
		for _, sql := range v.Setup {
			if _, err := conn.Exec(ctx, expandSQL(sql, runID)); err != nil {
				return fmt.Errorf("validate setup failed: %w", err)
			}
		}
		switch {
		case v.ReturnsValue != nil:
			return returnsValue(expandSQL(v.ReturnsValue.SQL, runID), v.ReturnsValue.Equals)(ctx, conn)
		case v.ExplainUses != nil:
			return explainUses(expandSQL(v.ExplainUses.SQL, runID), v.ExplainUses.NodeType, v.ExplainUses.IndexName)(ctx, conn)
		case v.ExplainContains != nil:
			return runExplainContains(ctx, conn, expandSQL(v.ExplainContains.SQL, runID), v.ExplainContains.Contains, v.ExplainContains.Excludes)
		}
		return nil
	}
}

// runExplainContains checks that EXPLAIN JSON contains all required substrings and none of the excluded ones.
func runExplainContains(ctx context.Context, conn *pgx.Conn, sql string, contains, excludes []string) error {
	var explainJSON string
	if err := conn.QueryRow(ctx, sql).Scan(&explainJSON); err != nil {
		return fmt.Errorf("EXPLAIN query failed: %w", err)
	}
	for _, s := range contains {
		if !strings.Contains(explainJSON, s) {
			return fmt.Errorf("EXPLAIN output missing %q", s)
		}
	}
	for _, s := range excludes {
		if strings.Contains(explainJSON, s) {
			return fmt.Errorf("EXPLAIN output unexpectedly contains %q", s)
		}
	}
	return nil
}
