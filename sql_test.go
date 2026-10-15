package dm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// sqlStep describes one expected database call. No network connection is opened.
type sqlStep struct {
	sql    string
	args   []driver.Value
	rows   *sqlRows
	result sqlResult
	err    error
}

type sqlResult struct {
	id, affected       int64
	idErr, affectedErr error
}

func (r sqlResult) LastInsertId() (int64, error) { return r.id, r.idErr }
func (r sqlResult) RowsAffected() (int64, error) { return r.affected, r.affectedErr }

type sqlRows struct {
	columns []string
	values  [][]driver.Value
	types   []string
	err     error
}

func (r *sqlRows) Columns() []string { return r.columns }
func (r *sqlRows) Close() error      { return nil }
func (r *sqlRows) Next(dest []driver.Value) error {
	if len(r.values) == 0 {
		if r.err != nil {
			return r.err
		}
		return io.EOF
	}
	copy(dest, r.values[0])
	r.values = r.values[1:]
	return nil
}

func (r *sqlRows) ColumnTypeDatabaseTypeName(index int) string {
	if len(r.types) == 0 {
		return dbTypeVarchar
	}
	return r.types[index]
}

func (*sqlRows) ColumnTypeNullable(int) (bool, bool) { return true, true }

type sqlScript struct {
	t     *testing.T
	steps []sqlStep
}

func (s *sqlScript) Connect(context.Context) (driver.Conn, error) { return s, nil }
func (s *sqlScript) Driver() driver.Driver                        { return s }
func (s *sqlScript) Open(string) (driver.Conn, error)             { return s, nil }
func (*sqlScript) Close() error                                   { return nil }
func (*sqlScript) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected Prepare")
}
func (*sqlScript) Begin() (driver.Tx, error) { return nil, errors.New("unexpected Begin") }

func (s *sqlScript) next(query string, args []driver.NamedValue, wantRows bool) sqlStep {
	s.t.Helper()
	if len(s.steps) == 0 {
		s.t.Fatalf("unexpected database call: %s, args=%v", query, args)
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	if !strings.Contains(query, step.sql) {
		s.t.Fatalf("SQL = %q, want it to contain %q", query, step.sql)
	}
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	if len(values) != len(step.args) || (len(values) > 0 && !reflect.DeepEqual(values, step.args)) {
		s.t.Fatalf("SQL %q args = %v, want %v", query, values, step.args)
	}
	if step.err == nil && wantRows != (step.rows != nil) {
		s.t.Fatalf("SQL %q used wrong execution method: query=%v", query, wantRows)
	}
	return step
}

func (s *sqlScript) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	step := s.next(query, args, false)
	return step.result, step.err
}

func (s *sqlScript) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	step := s.next(query, args, true)
	if step.err != nil {
		return nil, step.err
	}
	return step.rows, nil
}

func openScriptedDB(t *testing.T, mode int, steps ...sqlStep) *gorm.DB {
	t.Helper()
	script := &sqlScript{t: t, steps: steps}
	pool := sql.OpenDB(script)
	db, err := gorm.Open(New(Config{Conn: pool, GormMode: mode}), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
		if len(script.steps) > 0 {
			t.Errorf("%d expected database calls were not made; next: %s", len(script.steps), script.steps[0].sql)
		}
	})
	return db
}
