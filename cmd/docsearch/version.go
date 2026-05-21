package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
)

// These are overridden via -ldflags "-X main.buildVersion=..." at
// build time. Defaults are filled in from the embedded vcs info so
// `go build` without ldflags still produces sensible output.
var (
	buildVersion = ""
	buildTime    = ""
)

type versionInfo struct {
	BuildVersion string `json:"build_version"`
	BuildTime    string `json:"build_time"`
	GoVersion    string `json:"go_version"`
	DocsRef      string `json:"docs_ref,omitempty"`
	DocsRepo     string `json:"docs_repo,omitempty"`
	DocsBuiltAt  string `json:"docs_built_at,omitempty"`
}

func collectVersionInfo(db *sql.DB) versionInfo {
	v := versionInfo{
		BuildVersion: resolvedBuildVersion(),
		BuildTime:    buildTime,
		GoVersion:    runtime.Version(),
	}
	if db != nil {
		v.DocsRef = readMeta(db, "docs_ref")
		v.DocsRepo = readMeta(db, "docs_repo")
		v.DocsBuiltAt = readMeta(db, "built_at")
	}
	return v
}

// resolvedBuildVersion prefers the -ldflags value if set; otherwise
// it falls back to the vcs.revision embedded by `go build` since 1.18.
func resolvedBuildVersion() string {
	if buildVersion != "" {
		return buildVersion
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				return shortRev(s.Value)
			}
		}
	}
	return "unknown"
}

func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

func readMeta(db *sql.DB, key string) string {
	var value string
	// Older DBs (built before the meta table existed) will return
	// SQL errors; treat any failure as "unknown" rather than crashing
	// `docsearch version`.
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value); err != nil {
		return ""
	}
	return value
}

// maybeRunVersion handles `docsearch version`. Returns true when it
// owned the invocation (caller should stop processing flags).
func maybeRunVersion(args []string) bool {
	if len(args) <= 1 || args[1] != "version" {
		return false
	}
	// Open the configured DB if present; meta lookups gracefully
	// degrade if the file doesn't exist or lacks the table.
	dbPath := defaultDBPath()
	jsonOut := false
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "-d":
			if i+1 < len(args) {
				dbPath = args[i+1]
				i++
			}
		case "--json", "-json":
			jsonOut = true
		}
	}
	var db *sql.DB
	if _, err := os.Stat(dbPath); err == nil {
		opened, err := sql.Open("sqlite3", dbPath)
		if err == nil {
			db = opened
			defer db.Close()
		}
	}
	info := collectVersionInfo(db)
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(info)
		return true
	}
	fmt.Printf("docsearch  %s\n", info.BuildVersion)
	if info.BuildTime != "" {
		fmt.Printf("built      %s\n", info.BuildTime)
	}
	fmt.Printf("go         %s\n", info.GoVersion)
	if info.DocsRef != "" {
		fmt.Printf("docs ref   %s\n", info.DocsRef)
	}
	if info.DocsRepo != "" {
		fmt.Printf("docs repo  %s\n", info.DocsRepo)
	}
	if info.DocsBuiltAt != "" {
		fmt.Printf("docs built %s\n", info.DocsBuiltAt)
	}
	if info.DocsRef == "" && info.DocsRepo == "" && info.DocsBuiltAt == "" {
		fmt.Fprintf(os.Stderr, "(no meta table in %s; rebuild with bundle-docs to record docs ref)\n", dbPath)
	}
	return true
}
