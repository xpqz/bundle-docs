package main

import (
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestCollectVersionInfoReadsMetaTableWhenPresent(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	mustExec(t, db, `CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`)
	mustExec(t, db, `INSERT INTO meta VALUES ('docs_ref', 'abcdef1234567890')`)
	mustExec(t, db, `INSERT INTO meta VALUES ('docs_repo', 'https://github.com/Dyalog/documentation.git')`)
	mustExec(t, db, `INSERT INTO meta VALUES ('built_at', '2026-05-21T10:00:00Z')`)

	got := collectVersionInfo(db)
	if got.DocsRef != "abcdef1234567890" {
		t.Errorf("DocsRef = %q, want abcdef1234567890", got.DocsRef)
	}
	if got.DocsRepo != "https://github.com/Dyalog/documentation.git" {
		t.Errorf("DocsRepo = %q", got.DocsRepo)
	}
	if got.DocsBuiltAt != "2026-05-21T10:00:00Z" {
		t.Errorf("DocsBuiltAt = %q", got.DocsBuiltAt)
	}
	if got.GoVersion == "" {
		t.Errorf("GoVersion should not be empty")
	}
	if got.BuildVersion == "" {
		t.Errorf("BuildVersion should fall back to vcs revision or 'unknown'")
	}
}

func TestCollectVersionInfoToleratesMissingMetaTable(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	// No meta table; collectVersionInfo must not crash.
	got := collectVersionInfo(db)
	if got.DocsRef != "" || got.DocsRepo != "" || got.DocsBuiltAt != "" {
		t.Errorf("expected empty docs metadata when meta table absent: %#v", got)
	}
}

func TestCollectVersionInfoToleratesNilDB(t *testing.T) {
	got := collectVersionInfo(nil)
	if got.DocsRef != "" {
		t.Errorf("nil db should yield empty docs fields: %#v", got)
	}
	if got.GoVersion == "" {
		t.Errorf("GoVersion always populated")
	}
}

func TestResolvedBuildVersionPrefersLdflagValueWhenSet(t *testing.T) {
	saved := buildVersion
	t.Cleanup(func() { buildVersion = saved })

	buildVersion = "v0.0.0-test"
	if got := resolvedBuildVersion(); got != "v0.0.0-test" {
		t.Fatalf("resolvedBuildVersion = %q, want v0.0.0-test", got)
	}

	buildVersion = ""
	// With no ldflag, we should still get *some* value (either vcs
	// revision or the "unknown" fallback). Non-empty is enough.
	if got := resolvedBuildVersion(); got == "" {
		t.Fatalf("resolvedBuildVersion empty without ldflag fallback")
	}
}

func mustExec(t *testing.T, db *sql.DB, q string) {
	t.Helper()
	if _, err := db.Exec(q); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}
