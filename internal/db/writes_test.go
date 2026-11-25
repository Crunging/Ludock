package db

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// While Ludock updates itself, a write begun before the close finishes
// before it returns, and every other write is refused wherever it begins,
// so a rollback to the save taken then loses nothing that was acknowledged.
// Reads, and the update's own writes, go on.
func TestWritesCloseForAnUpdate(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "ludock.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	if _, err := d.Exec("CREATE TABLE numbers (n INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}

	// Begun before the close: it ends before the close returns.
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	// It would commit later, whenever it ends.
	if _, err := readOnly.Exec("INSERT INTO numbers VALUES (3)"); !errors.Is(err, errReadOnlyWrite) {
		t.Errorf("a read-only transaction wrote: %v", err)
	}
	closed := make(chan func())
	go func() {
		reopen, err := d.CloseWrites(ctx)
		if err != nil {
			t.Error(err)
		}
		closed <- reopen
	}()
	select {
	case <-closed:
		t.Fatal("writes closed while one was open")
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := tx.Exec("INSERT INTO numbers VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	reopen := <-closed

	refused := map[string]func() error{
		"transaction": func() error { _, err := d.BeginTx(ctx, nil); return err },
		"autocommit":  func() error { _, err := d.Exec("INSERT INTO numbers VALUES (2)"); return err },
		"writing CTE": func() error {
			_, err := d.Exec("WITH two(n) AS (SELECT 2) INSERT INTO numbers SELECT n FROM two")
			return err
		},
		"writing query": func() error {
			rows, err := d.Query("DELETE FROM numbers RETURNING n")
			if err == nil {
				rows.Close()
			}
			return err
		},
		"optimize": func() error { _, err := d.Exec("PRAGMA optimize"); return err },
		// Some PRAGMAs act as they are prepared.
		"prepared": func() error {
			stmt, err := d.Prepare("PRAGMA schema_version = 7")
			if err == nil {
				stmt.Close()
			}
			return err
		},
		"pragma call": func() error { _, err := d.Exec("PRAGMA user_version(42)"); return err },
	}
	for name, write := range refused {
		if err := write(); !errors.Is(err, ErrUpdating) {
			t.Errorf("%s: %v", name, err)
		}
	}
	readOnly.Rollback()
	var count int
	if err := d.QueryRow("SELECT COUNT(*) FROM numbers").Scan(&count); err != nil || count != 1 {
		t.Errorf("read %d %v", count, err)
	}
	if _, err := d.ExecContext(Maintenance(ctx), "INSERT INTO numbers VALUES (4)"); err != nil {
		t.Errorf("the update's own write: %v", err)
	}
	reopen()
	if _, err := d.Exec("INSERT INTO numbers VALUES (5)"); err != nil {
		t.Errorf("reopened: %v", err)
	}
}
