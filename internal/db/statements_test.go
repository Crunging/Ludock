package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"modernc.org/sqlite"
)

func statementDB(t *testing.T) *sql.DB {
	t.Helper()
	pool := sql.OpenDB(statementConnector{driver: &sqlite.Driver{},
		dsn: "file:" + filepath.Join(t.TempDir(), "statements.db") + "?_txlock=immediate"})
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { pool.Close() })
	if _, err := pool.Exec("CREATE TABLE numbers (n INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		if _, err := pool.Exec("INSERT INTO numbers VALUES (?)", n); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func cachedStatements(t *testing.T, pool *sql.DB) map[string]*cachedStatement {
	t.Helper()
	conn, err := pool.Conn(bg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var statements map[string]*cachedStatement
	conn.Raw(func(raw any) error {
		statements = raw.(*statementConn).statements
		return nil
	})
	return statements
}

// A transaction reads a query's rows and runs the same query for each row.
// The nested query must get its own statement instead of resetting the open
// rows, and the cached statement must be reusable afterwards.
func TestStatementCacheNestedQuery(t *testing.T) {
	pool := statementDB(t)
	const query = "SELECT n FROM numbers WHERE n >= ? ORDER BY n"
	tx, err := pool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(query, 1)
	if err != nil {
		t.Fatal(err)
	}
	var outer, inner []int
	for rows.Next() {
		var n int
		rows.Scan(&n)
		outer = append(outer, n)
		var first int
		if err := tx.QueryRow(query, n).Scan(&first); err != nil {
			t.Fatal(err)
		}
		inner = append(inner, first)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(outer, inner) != "[1 2 3] [1 2 3]" {
		t.Fatal(outer, inner)
	}
	var count int
	if err := tx.QueryRow(query, 3).Scan(&count); err != nil || count != 3 {
		t.Fatal(count, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if cached := cachedStatements(t, pool)[query]; cached == nil || cached.busy {
		t.Fatal("query is not cached and idle")
	}
}

// Statements keep working after errors, rows abandoned early, and writes, and
// the cache stays bounded when every query is a new string.
func TestStatementCacheReuseAndBound(t *testing.T) {
	pool := statementDB(t)
	for range 3 {
		if _, err := pool.Exec("INSERT INTO numbers VALUES (?)", nil); err == nil {
			t.Fatal("NOT NULL was not enforced")
		}
		rows, err := pool.Query("SELECT n FROM numbers ORDER BY n")
		if err != nil {
			t.Fatal(err)
		}
		rows.Next()
		rows.Close()
		if _, err := pool.Exec("UPDATE numbers SET n = n + 1 WHERE n = ?", 1); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	if err := pool.QueryRow("SELECT sum(n) FROM numbers").Scan(&total); err != nil || total != 2+2+3 {
		t.Fatal(total, err)
	}
	for n := range maxStatements + 10 {
		var got int
		if err := pool.QueryRow(fmt.Sprintf("SELECT %d", n)).Scan(&got); err != nil || got != n {
			t.Fatal(got, err)
		}
	}
	if count := len(cachedStatements(t, pool)); count != maxStatements {
		t.Fatal(count)
	}
}
