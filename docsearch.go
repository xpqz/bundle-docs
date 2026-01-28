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
	limit := flag.Int("l", 10, "maximum number of results")
	flag.Parse()

	if *search == "" && *rowid == 0 {
		fmt.Fprintln(os.Stderr, "Usage: docsearch -s <search> | -r <rowid>")
		fmt.Fprintln(os.Stderr, "  -d <database>  Database path (default: ./dyalog-docs.db)")
		fmt.Fprintln(os.Stderr, "  -s <string>    Search string (use '-' to read from stdin)")
		fmt.Fprintln(os.Stderr, "  -r <rowid>     Fetch document by rowid")
		fmt.Fprintln(os.Stderr, "  -l <limit>     Maximum number of results (default: 10)")
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

	searchDocs(db, query, *limit)
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

func searchDocs(db *sql.DB, query string, limit int) {
	seen := make(map[int64]bool)
	count := 0

	// 1. Exact case-insensitive match on keywords
	rows, err := db.Query(`
		SELECT rowid, title FROM docs
		WHERE keywords LIKE ? COLLATE NOCASE AND exclude = 0
	`, "%"+query+"%")
	if err != nil {
		log.Fatal(err)
	}
	count = printResults(rows, seen, limit, count)
	if count >= limit {
		return
	}

	// 2. FTS search on title
	rows, err = db.Query(`
		SELECT f.rowid, f.title FROM docs_fts f
		JOIN docs d ON f.rowid = d.rowid
		WHERE f.title MATCH ? AND d.exclude = 0
	`, escapeQuery(query))
	if err != nil {
		log.Fatal(err)
	}
	count = printResults(rows, seen, limit, count)
	if count >= limit {
		return
	}

	// 3. FTS search on content
	rows, err = db.Query(`
		SELECT f.rowid, f.title FROM docs_fts f
		JOIN docs d ON f.rowid = d.rowid
		WHERE f.content MATCH ? AND d.exclude = 0
	`, escapeQuery(query))
	if err != nil {
		log.Fatal(err)
	}
	printResults(rows, seen, limit, count)
}

func printResults(rows *sql.Rows, seen map[int64]bool, limit, count int) int {
	defer rows.Close()
	for rows.Next() {
		if count >= limit {
			break
		}
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
		count++
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	return count
}

// escapeQuery wraps the query in quotes to handle special characters
func escapeQuery(q string) string {
	// Escape double quotes by doubling them
	q = strings.ReplaceAll(q, `"`, `""`)
	return `"` + q + `"`
}
