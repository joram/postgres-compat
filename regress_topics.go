package main

const (
	bce = "behavior_and_core_experience"
	fn  = "functionality"
	bar = "backups_and_replication"
)

// regressOptionals lists upstream pg_regress tests that legitimately fail or
// behave differently on a stock PostgreSQL install — they need a non-default
// GUC, a build-time library (libxml2), an extension, superuser-only DDL, or
// wal_level=logical. These are reported as Optional so they don't fail the
// Core gate. Everything else is Core under behavior_and_core_experience/
// sql_regression. The reasoning per entry stays in the comments so the list
// is auditable; the structure does not encode a Riga sub-taxonomy beyond
// domain (downstream renderers can re-group tests by name if desired).
var regressOptionals = map[string]struct {
	domain, topic string
}{
	"event_trigger":           {bce, "sql_compliance"},        // server-level, needs superuser
	"event_trigger_login":     {bce, "sql_compliance"},        // server-level, needs superuser
	"tablespace":              {bce, "sql_compliance"},        // needs directory creation
	"prepared_xacts":          {bce, "transaction_isolation"}, // needs max_prepared_transactions > 0
	"password":                {bce, "connectivity"},          // needs SCRAM/MD5 GUCs that vary per deployment
	"security_label":          {bce, "connectivity"},          // needs sepgsql or similar provider
	"largeobject":             {fn, "data_types"},             // needs file access on client
	"xml":                     {fn, "data_types"},             // needs libxml2 build flag
	"xmlmap":                  {fn, "data_types"},             // needs libxml2 build flag
	"collate.linux.utf8":      {fn, "data_types"},             // platform-specific locale
	"collate.icu.utf8":        {fn, "data_types"},             // ICU build flag
	"collate.utf8":            {fn, "data_types"},             // platform-specific locale
	"collate.windows.win1252": {fn, "data_types"},             // Windows-only
	"foreign_data":            {fn, "feature_dependencies"},   // needs FDW extension
	"publication":             {bar, "logical_replication"},   // needs wal_level=logical
	"subscription":            {bar, "logical_replication"},   // needs wal_level=logical
	"replica_identity":        {bar, "logical_replication"},   // needs wal_level=logical
}

// regressTopicFor returns the (domain, topic, core) classification for an
// upstream pg_regress test name. The vast majority of tests collapse to a
// single bucket — behavior_and_core_experience/sql_regression — and the per-
// test name preserves distinguishing information for downstream renderers.
// Only the legitimately-Optional set carries explicit per-test domain/topic.
func regressTopicFor(name string) (domain, topic string, core bool) {
	if opt, ok := regressOptionals[name]; ok {
		return opt.domain, opt.topic, false
	}
	return bce, "sql_regression", true
}
