package db

import (
	"context"
	"database/sql/driver"
	"errors"
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
	// inTx: a transaction admitted by the write gate is open.
	inTx bool
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
		return &cachedStatement{stmt: stmt, busy: true, conn: c, writes: writes(query), transient: true}, nil
	}
	stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	cached := &cachedStatement{stmt: stmt, busy: true, conn: c, writes: writes(query)}
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
func (c *statementConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	leave, err := c.writes.enter(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
	if err != nil {
		leave()
		return nil, err
	}
	c.inTx = true
	return &gatedTx{Tx: tx, end: func() { c.inTx = false; leave() }}, nil
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
	// admitted through the write gate.
	writes bool
	// transient statements aren't kept, so closing them finalizes them.
	transient bool
}

func (s *cachedStatement) admit(ctx context.Context) (func(), error) {
	if !s.writes || s.conn.inTx {
		return func() {}, nil
	}
	return s.conn.writes.enter(ctx)
}

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
	leave, err := s.admit(context.Background())
	if err != nil {
		return nil, err
	}
	defer leave()
	//lint:ignore SA1019 driver.Stmt requires Exec
	return s.stmt.Exec(args)
}

func (s *cachedStatement) Query(args []driver.Value) (driver.Rows, error) {
	leave, err := s.admit(context.Background())
	if err != nil {
		return nil, err
	}
	defer leave()
	//lint:ignore SA1019 driver.Stmt requires Query
	return s.stmt.Query(args)
}

func (s *cachedStatement) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	leave, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer leave()
	return s.stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

// QueryContext admits a writing query, such as DELETE … RETURNING, while it
// runs: SQLite commits it when its rows are first read.
func (s *cachedStatement) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	leave, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		leave()
		return nil, err
	}
	return &gatedRows{Rows: rows, leave: leave}, nil
}

type gatedRows struct {
	driver.Rows
	leave func()
}

func (r *gatedRows) Close() error {
	defer r.leave()
	return r.Rows.Close()
}
