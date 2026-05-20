package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestCase defines a single test for the compatibility matrix.
// Domain and Topic are raw identifiers derived from the suite directory layout
// (directory name and YAML filename stem respectively).
type TestCase struct {
	ID                string
	Domain            string
	Topic             string
	Description       string
	Core              bool
	Alter             bool // true if the test writes to the database (DDL/DML); false = read-only safe
	Setup             []string
	Probe             []string
	ExpectError       string // optional SQLSTATE; "" means probe must succeed on last statement
	Validate          func(ctx context.Context, conn *pgx.Conn) error
	Teardown          []string
	Serial            bool
	SupportedVersions []int
	Extension         string // if set, test is skipped unless CREATE EXTENSION <name> succeeded at startup
}

// Result tracks the outcome of a TestCase. Score is not stored: it's a
// policy decision computed at aggregation time via Score().
type Result struct {
	Test          TestCase
	Skipped       bool
	SkipReason    string
	DurationMS    int64
	SilentFailure bool
	SQLState      string
	Err           error
}

// Passed reports whether the test succeeded. Pass/fail is binary by design:
// weighting between tests is qualitative (Core gates the exit code, Optional
// does not), not a per-test fractional score.
func (r Result) Passed() bool {
	return r.Err == nil
}

// AppliesTo reports whether the test should run against the given PG major
// version. An empty SupportedVersions means "applies to all versions."
func (tc TestCase) AppliesTo(major int) bool {
	if len(tc.SupportedVersions) == 0 {
		return true
	}
	for _, v := range tc.SupportedVersions {
		if v == major {
			return true
		}
	}
	return false
}

// sqlStateOf returns the SQLSTATE code from a pgx PgError if err is one,
// otherwise the empty string. Used to tag Results so managed-service
// permission errors (42501) are distinguishable from feature gaps.
func sqlStateOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// returnsValue is a Validate helper that checks if a query returns a single specific value.
func returnsValue(sql string, equals string) func(ctx context.Context, conn *pgx.Conn) error {
	return func(ctx context.Context, conn *pgx.Conn) error {
		var val string
		err := conn.QueryRow(ctx, sql).Scan(&val)
		if err != nil {
			return fmt.Errorf("returnsValue query failed: %w", err)
		}
		if val != equals {
			return fmt.Errorf("expected %s, got %s", equals, val)
		}
		return nil
	}
}

// explainUses is a Validate helper that checks an EXPLAIN plan contains a specific node type and/or index.
func explainUses(sql, nodeType, indexName string) func(ctx context.Context, conn *pgx.Conn) error {
	return func(ctx context.Context, conn *pgx.Conn) error {
		var explainJSON string
		err := conn.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql).Scan(&explainJSON)
		if err != nil {
			return fmt.Errorf("EXPLAIN query failed: %w", err)
		}
		if !strings.Contains(explainJSON, `"`+nodeType+`"`) {
			return fmt.Errorf("EXPLAIN did not use node type %s. JSON: %s", nodeType, explainJSON)
		}
		if indexName != "" && !strings.Contains(explainJSON, `"`+indexName+`"`) {
			return fmt.Errorf("EXPLAIN did not use index %s. JSON: %s", indexName, explainJSON)
		}
		return nil
	}
}
