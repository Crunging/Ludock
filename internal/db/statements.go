package db

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync/atomic"

	"modernc.org/sqlite"
)

// maxStatements bounds each connection's parsed statements. Ludock's queries
// are fixed strings, so this is only reached by queries built per call.
const maxStatements = 256

// statementConnector opens SQLite connections that keep their parsed
// statements; database/sql otherwise reparses every query on every call, which
// was most of the CPU of a server detail read.
type statementConnector struct {
	driver *sqlite.Driver
	dsn    string
	writes *writeGate
}

func (c statementConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &statementConn{Conn: conn, statements: map[string]*cachedStatement{}, writes: c.writes}, nil
}

func (c statementConnector) Driver() driver.Driver { return c.driver }

// statementConn has no QueryContext or ExecContext, so database/sql prepares
// each query through PrepareContext and closes the statement when its rows
// or result are done. database/sql uses a connection from one goroutine at a
// time, so the map needs no lock.
type statementConn struct {
	driver.Conn
	statements map[string]*cachedStatement
	writes     *writeGate
	// inTx: a transaction the write gate admitted is open; readOnly: one
	// it didn't, in which no write may run, since it would commit later.
	inTx, readOnly bool
}

var (
	_ driver.ConnPrepareContext = (*statementConn)(nil)
	_ driver.ConnBeginTx        = (*statementConn)(nil)
	_ driver.SessionResetter    = (*statementConn)(nil)
	_ driver.Validator          = (*statementConn)(nil)
	_ driver.Pinger             = (*statementConn)(nil)
)

func (c *statementConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// statements counts the statements run since the process started.
var statements atomic.Int64

// Statements is how many SQL statements Ludock has run, for tests and
// measurements that count a request's queries.
func Statements() int64 { return statements.Load() }

func (c *statementConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	statements.Add(1)
	if cached := c.statements[query]; cached != nil {
		if !cached.busy {
			cached.busy = true
			return cached, nil
		}
		// The same query nested inside its own open rows needs a second
		// statement; resetting the first would end those rows.
		stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
		if err != nil {
			return nil, err
		}
		return &cachedStatement{stmt: stmt, busy: true, conn: c, writes: writes(query), changes: changes(query), transient: true}, nil
	}
	// Some PRAGMAs act as SQLite compiles them, so a write is refused
	// before it is prepared, not only when it runs.
	if !c.inTx && writes(query) {
		if c.readOnly {
			return nil, errReadOnlyWrite
		}
		if c.writes.refuses(ctx) {
			return nil, ErrUpdating
		}
	}
	stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	cached := &cachedStatement{stmt: stmt, busy: true, conn: c, writes: writes(query), changes: changes(query)}
	if len(c.statements) >= maxStatements && !c.evict() {
		cached.transient = true
		return cached, nil
	}
	c.statements[query] = cached
	return cached, nil
}

// evict finalizes one idle statement, reporting whether there was one.
func (c *statementConn) evict() bool {
	for query, cached := range c.statements {
		if !cached.busy {
			cached.stmt.Close()
			delete(c.statements, query)
			return true
		}
	}
	return false
}

func (c *statementConn) Close() error {
	var errs []error
	for query, cached := range c.statements {
		errs = append(errs, cached.stmt.Close())
		delete(c.statements, query)
	}
	return errors.Join(append(errs, c.Conn.Close())...)
}

// BeginTx admits every transaction through the write gate: each is
// immediate, taking SQLite's write lock.
// A read-only transaction, such as for a consistent page of Activity, isn't
// admitted: its statements are, one by one, as outside a transaction.
func (c *statementConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	leave := func() {}
	if !options.ReadOnly {
		var err error
		if leave, err = c.writes.enter(ctx); err != nil {
			return nil, err
		}
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		leave()
		return nil, err
	}
	if !options.ReadOnly && !maintenance(ctx) {
		// Holding the write lock, a transaction of any process sees the
		// record the update writes before its data save, so it can't
		// commit after that save; a rollback would undo it.
		if updating, err := c.updating(ctx); err != nil || updating {
			tx.Rollback()
			leave()
			if err == nil {
				err = ErrUpdating
			}
			return nil, err
		}
	}
	c.inTx, c.readOnly = !options.ReadOnly, options.ReadOnly
	return &gatedTx{Tx: tx, end: func() { c.inTx, c.readOnly = false, false; leave() }}, nil
}

// UpdateSetting is the setting that holds Ludock's latest self-update.
const UpdateSetting = "ludock.update"

// updating reports whether the database records a self-update that handed
// over, after which nothing but the update writes.
func (c *statementConn) updating(ctx context.Context) (bool, error) {
	stmt, err := c.PrepareContext(ctx, "SELECT 1 FROM settings WHERE key = ? AND json_extract(value_json, '$.state') = 'handed_over'")
	if err != nil && strings.Contains(err.Error(), "no such table: settings") {
		// A database without settings has never begun an update.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	rows, err := stmt.(driver.StmtQueryContext).QueryContext(ctx, []driver.NamedValue{{Ordinal: 1, Value: UpdateSetting}})
	if err != nil {
		return false, err
	}
	defer rows.Close()
	err = rows.Next(make([]driver.Value, 1))
	if err == io.EOF {
		return false, nil
	}
	return err == nil, err
}

type gatedTx struct {
	driver.Tx
	end func()
}

func (t *gatedTx) Commit() error {
	defer t.end()
	return t.Tx.Commit()
}

func (t *gatedTx) Rollback() error {
	defer t.end()
	return t.Tx.Rollback()
}

func (c *statementConn) ResetSession(ctx context.Context) error {
	return c.Conn.(driver.SessionResetter).ResetSession(ctx)
}

func (c *statementConn) IsValid() bool { return c.Conn.(driver.Validator).IsValid() }

func (c *statementConn) Ping(ctx context.Context) error { return c.Conn.(driver.Pinger).Ping(ctx) }

// cachedStatement stays prepared when database/sql closes it. The driver
// resets a single statement after each use, ready for the next.
type cachedStatement struct {
	stmt driver.Stmt
	busy bool
	conn *statementConn
	// writes: the statement may write, so outside a transaction it is
	// admitted through the write gate; changes: it changes rows, so outside
	// a transaction it runs in its own, which checks for an update.
	writes, changes bool
	// transient statements aren't kept, so closing them finalizes them.
	transient bool
}

// admit admits a write outside a transaction. One that changes rows then
// runs in its own transaction, which end commits; a statement that can't
// run in a transaction, such as VACUUM, is admitted by this process's gate
// alone.
func (s *cachedStatement) admit(ctx context.Context) (end func(failed bool) error, err error) {
	if !s.writes || s.conn.inTx {
		return func(bool) error { return nil }, nil
	}
	if s.conn.readOnly {
		return nil, errReadOnlyWrite
	}
	if !s.changes || maintenance(ctx) {
		leave, err := s.conn.writes.enter(ctx)
		if err != nil {
			return nil, err
		}
		return func(bool) error { leave(); return nil }, nil
	}
	tx, err := s.conn.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		return nil, err
	}
	return func(failed bool) error {
		if failed {
			return tx.Rollback()
		}
		return tx.Commit()
	}, nil
}

var errReadOnlyWrite = errors.New("a read-only transaction can't write")

var (
	_ driver.StmtExecContext  = (*cachedStatement)(nil)
	_ driver.StmtQueryContext = (*cachedStatement)(nil)
)

func (s *cachedStatement) Close() error {
	s.busy = false
	if s.transient {
		return s.stmt.Close()
	}
	return nil
}

func (s *cachedStatement) NumInput() int { return s.stmt.NumInput() }

// Exec and Query complete driver.Stmt; database/sql calls the Context forms.
func (s *cachedStatement) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func (s *cachedStatement) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

func named(args []driver.Value) []driver.NamedValue {
	values := make([]driver.NamedValue, len(args))
	for index, arg := range args {
		values[index] = driver.NamedValue{Ordinal: index + 1, Value: arg}
	}
	return values
}

func (s *cachedStatement) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	end, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	result, err := s.stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	if endErr := end(err != nil); err == nil && endErr != nil {
		return nil, endErr
	}
	return result, err
}

// QueryContext admits a writing query, such as DELETE … RETURNING, until its
// rows are closed.
func (s *cachedStatement) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	end, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		end(true)
		return nil, err
	}
	return &gatedRows{Rows: rows, end: end}, nil
}

type gatedRows struct {
	driver.Rows
	end func(failed bool) error
}

func (r *gatedRows) Close() error {
	err := r.Rows.Close()
	return errors.Join(err, r.end(err != nil))
}
