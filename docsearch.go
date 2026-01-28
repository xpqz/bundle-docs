// docsearch queries the Dyalog documentation database.
//
//	go build -tags "fts5" -o docsearch docsearch.go
package main

import (
	"bufio"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	dbPath := flag.String("d", "./dyalog-docs.db", "database path")
	search := flag.String("s", "", "search string (use '-' to read from stdin)")
	rowid := flag.Int64("r", 0, "fetch document by rowid")
	flag.Parse()

	if *search == "" && *rowid == 0 {
		fmt.Fprintln(os.Stderr, "Usage: docsearch -s <search> | -r <rowid>")
		fmt.Fprintln(os.Stderr, "  -d <database>  Database path (default: ./dyalog-docs.db)")
		fmt.Fprintln(os.Stderr, "  -s <string>    Search string (use '-' to read from stdin)")
		fmt.Fprintln(os.Stderr, "  -r <rowid>     Fetch document by rowid")
		os.Exit(1)
	}

	db, err := sql.Open("sqlite3", *dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	if *rowid != 0 {
		fetchByRowid(db, *rowid)
		return
	}

	query := *search
	if query == "-" {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			query = scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			log.Fatal(err)
		}
	}

	if query == "" {
		log.Fatal("empty search string")
	}

	searchDocs(db, query)
}

func fetchByRowid(db *sql.DB, rowid int64) {
	var content string
	err := db.QueryRow("SELECT content FROM docs WHERE rowid = ?", rowid).Scan(&content)
	if err == sql.ErrNoRows {
		log.Fatalf("no document with rowid %d", rowid)
	}
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(content)
}

func searchDocs(db *sql.DB, query string) {
	seen := make(map[int64]bool)

	// 1. Exact case-insensitive match on keywords
	rows, err := db.Query(`
		SELECT rowid, title FROM docs
		WHERE keywords LIKE ? COLLATE NOCASE
	`, "%"+query+"%")
	if err != nil {
		log.Fatal(err)
	}
	printResults(rows, seen)

	// 2. FTS search on title
	rows, err = db.Query(`
		SELECT rowid, title FROM docs_fts
		WHERE title MATCH ?
	`, escapeQuery(query))
	if err != nil {
		log.Fatal(err)
	}
	printResults(rows, seen)

	// 3. FTS search on content
	rows, err = db.Query(`
		SELECT rowid, title FROM docs_fts
		WHERE content MATCH ?
	`, escapeQuery(query))
	if err != nil {
		log.Fatal(err)
	}
	printResults(rows, seen)
}

func printResults(rows *sql.Rows, seen map[int64]bool) {
	defer rows.Close()
	for rows.Next() {
		var rowid int64
		var title string
		if err := rows.Scan(&rowid, &title); err != nil {
			log.Fatal(err)
		}
		if seen[rowid] {
			continue
		}
		seen[rowid] = true
		fmt.Printf("%d %s\n", rowid, title)
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
}

// escapeQuery wraps the query in quotes to handle special characters
func escapeQuery(q string) string {
	// Escape double quotes by doubling them
	q = strings.ReplaceAll(q, `"`, `""`)
	return `"` + q + `"`
}
