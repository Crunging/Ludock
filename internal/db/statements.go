package db

import (
	"context"
	"database/sql/driver"
	"errors"

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
}

func (c statementConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &statementConn{Conn: conn, statements: map[string]*cachedStatement{}}, nil
}

func (c statementConnector) Driver() driver.Driver { return c.driver }

// statementConn has no QueryContext or ExecContext, so database/sql prepares
// each query through PrepareContext and closes the statement when its rows
// or result are done. database/sql uses a connection from one goroutine at a
// time, so the map needs no lock.
type statementConn struct {
	driver.Conn
	statements map[string]*cachedStatement
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

func (c *statementConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if cached := c.statements[query]; cached != nil {
		if !cached.busy {
			cached.busy = true
			return cached, nil
		}
		// The same query nested inside its own open rows needs a second
		// statement; resetting the first would end those rows.
		return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	}
	stmt, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
	if err != nil || (len(c.statements) >= maxStatements && !c.evict()) {
		return stmt, err
	}
	cached := &cachedStatement{stmt: stmt, busy: true}
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

func (c *statementConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
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
}

var (
	_ driver.StmtExecContext  = (*cachedStatement)(nil)
	_ driver.StmtQueryContext = (*cachedStatement)(nil)
)

func (s *cachedStatement) Close() error {
	s.busy = false
	return nil
}

func (s *cachedStatement) NumInput() int { return s.stmt.NumInput() }

// Exec and Query complete driver.Stmt; database/sql calls the Context forms.
func (s *cachedStatement) Exec(args []driver.Value) (driver.Result, error) {
	//lint:ignore SA1019 driver.Stmt requires Exec
	return s.stmt.Exec(args)
}

func (s *cachedStatement) Query(args []driver.Value) (driver.Rows, error) {
	//lint:ignore SA1019 driver.Stmt requires Query
	return s.stmt.Query(args)
}

func (s *cachedStatement) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.stmt.(driver.StmtExecContext).ExecContext(ctx, args)
}

func (s *cachedStatement) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
}
