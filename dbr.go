package dbr

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/mailru/dbr/dialect"
)

// Open instantiates a Connection for a given database/sql connection
// and event receiver
func Open(driver, dsn string, log EventReceiver) (*Connection, error) {
	if log == nil {
		log = nullReceiver
	}
	conn, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	var d Dialect
	switch driver {
	case "mysql":
		d = dialect.MySQL
	case "postgres":
		d = dialect.PostgreSQL
	case "sqlite3":
		d = dialect.SQLite3
	case "clickhouse":
		d = dialect.ClickHouse
	default:
		return nil, ErrNotSupported
	}
	return &Connection{DB: conn, EventReceiver: log, Dialect: d}, nil
}

const (
	placeholder = "?"
)

// Connection is a connection to the database with an EventReceiver
// to send events, errors, and timings to
type Connection struct {
	*sql.DB
	Dialect Dialect
	EventReceiver
}

// Session represents a business unit of execution for some connection
type Session struct {
	*Connection
	EventReceiver
	ctx context.Context
}

// NewSession instantiates a Session for the Connection
func (conn *Connection) NewSession(log EventReceiver) *Session {
	return conn.NewSessionContext(context.Background(), log)
}

// NewSessionContext instantiates a Session with context for the Connection
func (conn *Connection) NewSessionContext(ctx context.Context, log EventReceiver) *Session {
	if log == nil {
		log = conn.EventReceiver // Use parent instrumentation
	}
	return &Session{Connection: conn, EventReceiver: log, ctx: ctx}
}

// NewSession forks current session
func (sess *Session) NewSession(log EventReceiver) *Session {
	if log == nil {
		log = sess.EventReceiver
	}
	return &Session{Connection: sess.Connection, EventReceiver: log, ctx: sess.ctx}
}

// beginTx starts a transaction with context.
func (conn *Connection) beginTx() (*sql.Tx, error) {
	return conn.Begin()
}

// SessionRunner can do anything that a Session can except start a transaction.
type SessionRunner interface {
	Select(column ...string) SelectBuilder
	SelectBySql(query string, value ...interface{}) SelectBuilder

	InsertInto(table string) InsertBuilder
	InsertBySql(query string, value ...interface{}) InsertBuilder

	Update(table string) UpdateBuilder
	UpdateBySql(query string, value ...interface{}) UpdateBuilder

	DeleteFrom(table string) DeleteBuilder
	DeleteBySql(query string, value ...interface{}) DeleteBuilder
}

type runner interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)

	Query(query string, args ...interface{}) (*sql.Rows, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

// Executer can execute requests to database
type Executer interface {
	Exec() (sql.Result, error)
	ExecContext(ctx context.Context) (sql.Result, error)
}

type loader interface {
	Load(value interface{}) (int, error)
	LoadStruct(value interface{}) error
	LoadStructs(value interface{}) (int, error)
	LoadValue(value interface{}) error
	LoadValues(value interface{}) (int, error)
	LoadContext(ctx context.Context, value interface{}) (int, error)
	LoadStructContext(ctx context.Context, value interface{}) error
	LoadStructsContext(ctx context.Context, value interface{}) (int, error)
	LoadValueContext(ctx context.Context, value interface{}) error
	LoadValuesContext(ctx context.Context, value interface{}) (int, error)
}

func exec(ctx context.Context, runner runner, log EventReceiver, builder Builder, d Dialect) (sql.Result, error) {
	i := interpolator{
		Buffer:       NewBuffer(),
		Dialect:      d,
		IgnoreBinary: true,
	}
	err := i.interpolate(placeholder, []interface{}{builder})
	query, value := i.String(), i.Value()
	if err != nil {
		return nil, log.EventErrKv("dbr.exec.interpolate", err, kvs{
			"sql":  query,
			"args": fmt.Sprint(value),
		})
	}

	startTime := time.Now()
	defer func() {
		log.TimingKv("dbr.exec", time.Since(startTime).Nanoseconds(), kvs{
			"sql": query,
		})
	}()

	traceImpl, hasTracingImpl := log.(TracingEventReceiver)
	if hasTracingImpl {
		ctx = traceImpl.SpanStart(ctx, "dbr.exec", query)
		defer traceImpl.SpanFinish(ctx)
	}

	result, err := runner.Exec(query, value...)
	if err != nil {
		if hasTracingImpl {
			traceImpl.SpanError(ctx, err)
		}

		return result, log.EventErrKv("dbr.exec.exec", err, kvs{
			"sql": query,
		})
	}
	return result, nil
}

func query(ctx context.Context, runner runner, log EventReceiver, builder Builder, d Dialect, dest interface{}) (int, error) {
	i := interpolator{
		Buffer:       NewBuffer(),
		Dialect:      d,
		IgnoreBinary: true,
	}
	err := i.interpolate(placeholder, []interface{}{builder})
	query, value := i.String(), i.Value()
	if err != nil {
		return 0, log.EventErrKv("dbr.select.interpolate", err, kvs{
			"sql":  query,
			"args": fmt.Sprint(value),
		})
	}

	startTime := time.Now()
	defer func() {
		log.TimingKv("dbr.select", time.Since(startTime).Nanoseconds(), kvs{
			"sql": query,
		})
	}()

	traceImpl, hasTracingImpl := log.(TracingEventReceiver)
	if hasTracingImpl {
		ctx = traceImpl.SpanStart(ctx, "dbr.select", query)
		defer traceImpl.SpanFinish(ctx)
	}

	rows, err := runner.QueryContext(ctx, query, value...)
	if err != nil {
		if hasTracingImpl {
			traceImpl.SpanError(ctx, err)
		}

		return 0, log.EventErrKv("dbr.select.load.query", err, kvs{
			"sql": query,
		})
	}
	count, err := Load(rows, dest)
	if err != nil {
		return 0, log.EventErrKv("dbr.select.load.scan", err, kvs{
			"sql": query,
		})
	}
	return count, nil
}
