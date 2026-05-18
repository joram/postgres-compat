package main

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestResultPassed(t *testing.T) {
	if !(Result{}).Passed() {
		t.Error("Result with nil Err should be Passed")
	}
	if (Result{Err: errors.New("fail")}).Passed() {
		t.Error("Result with non-nil Err should not be Passed")
	}
}

func TestSuiteLoaded(t *testing.T) {
	if len(Tests) == 0 {
		t.Fatal("init() loaded no tests from suite/")
	}
	if len(DomainOrder) == 0 {
		t.Fatal("init() recorded no domains")
	}
	ids := make(map[string]bool)
	for _, tc := range Tests {
		if tc.ID == "" {
			t.Errorf("test has empty ID")
		}
		if tc.Domain == "" {
			t.Errorf("test %s has empty domain", tc.ID)
		}
		if tc.Topic == "" {
			t.Errorf("test %s has empty topic", tc.ID)
		}
		if len(tc.Probe) == 0 {
			t.Errorf("test %s has empty probe", tc.ID)
		}
		if ids[tc.ID] {
			t.Errorf("duplicate test ID: %s", tc.ID)
		}
		ids[tc.ID] = true
	}
}

func TestCompileDomains(t *testing.T) {
	saveDO, saveTO := DomainOrder, TopicOrder
	t.Cleanup(func() { DomainOrder, TopicOrder = saveDO, saveTO })
	DomainOrder = []string{"d1", "d2"}
	TopicOrder = map[string][]string{
		"d1": {"t_first", "t_second"},
		"d2": {"only"},
	}
	in := map[string]map[string][]TestOutput{
		"d1": {
			"t_second": {{ID: "b"}, {ID: "a"}},
			"t_first":  {{ID: "x"}},
		},
		"d2": {"only": {{ID: "z"}}},
	}
	got := compileDomains(in)
	if len(got) != 2 || got[0].ID != "d1" || got[1].ID != "d2" {
		t.Fatalf("domain ordering wrong: %+v", got)
	}
	if got[0].Topics[0].ID != "t_first" || got[0].Topics[1].ID != "t_second" {
		t.Fatalf("topic ordering wrong: %+v", got[0].Topics)
	}
	if got[0].Topics[1].Tests[0].ID != "a" {
		t.Fatalf("tests not sorted by ID: %+v", got[0].Topics[1].Tests)
	}
}

func TestExpandSQL(t *testing.T) {
	if got := expandSQL("CREATE ROLE t_{runid}", "abc123"); got != "CREATE ROLE t_abc123" {
		t.Errorf("expandSQL: got %q", got)
	}
	if expandSQL("SELECT 1", "x") != "SELECT 1" {
		t.Error("expandSQL should pass through SQL without placeholder")
	}
}

func TestSqlStateOf(t *testing.T) {
	if got := sqlStateOf(nil); got != "" {
		t.Errorf("sqlStateOf(nil) = %q, want empty", got)
	}
	if got := sqlStateOf(errors.New("plain")); got != "" {
		t.Errorf("sqlStateOf(plain err) = %q, want empty", got)
	}
	pgErr := &pgconn.PgError{Code: "42501", Severity: "ERROR", Message: "permission denied"}
	if got := sqlStateOf(pgErr); got != "42501" {
		t.Errorf("sqlStateOf(pgErr) = %q, want 42501", got)
	}
	wrapped := errors.Join(errors.New("context"), pgErr)
	if got := sqlStateOf(wrapped); got != "42501" {
		t.Errorf("sqlStateOf(wrapped pgErr) = %q, want 42501", got)
	}
}

func TestAppliesTo(t *testing.T) {
	cases := []struct {
		name      string
		supported []int
		major     int
		want      bool
	}{
		{"empty applies to any", nil, 17, true},
		{"empty applies to ancient", nil, 9, true},
		{"match", []int{15, 16, 17}, 17, true},
		{"no match", []int{15, 16, 17}, 14, false},
		{"single match", []int{14}, 14, true},
		{"single no match", []int{14}, 17, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tc := TestCase{SupportedVersions: c.supported}
			if got := tc.AppliesTo(c.major); got != c.want {
				t.Errorf("AppliesTo(%d) with %v = %v, want %v", c.major, c.supported, got, c.want)
			}
		})
	}
}

func TestMatchesFilter(t *testing.T) {
	if !matchesFilter("any_domain", "any_topic", "") {
		t.Error("empty filter should match everything")
	}
	if !matchesFilter("behavior_and_core_experience", "sql_compliance", "behavior") {
		t.Error("substring match against domain failed")
	}
	if !matchesFilter("functionality", "indexing", "INDEX") {
		t.Error("case-insensitive substring match against topic failed")
	}
	if matchesFilter("behavior_and_core_experience", "connectivity", "indexing") {
		t.Error("non-matching filter should reject")
	}
}
