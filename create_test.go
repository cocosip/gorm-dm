package dm

import (
	"context"
	"database/sql"

	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type testUser struct {
	ID   uint64 `gorm:"primaryKey;autoIncrement"`
	Name string `gorm:"size:64"`
	Age  int32
}

// Column/value literals shared by the create tests (and the integration
// tests in this package); repeated strings trip goconst.
const (
	colName      = "name"
	colAge       = "age"
	tableUsers   = "users"
	testUserName = "foo"
)

// openDryRun opens a DryRun gorm.DB without ever contacting a server: the
// dummy DSN parses (the dm driver validates it in sql.Open), but
// DisableAutomaticPing and DryRun prevent any connection.
func openDryRun(t *testing.T, gormMode int) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(New(Config{
		DSN:      "dm://dummy:dummy@127.0.0.1:5236?schema=dummy",
		GormMode: gormMode,
	}), &gorm.Config{
		DryRun:                   true,
		DisableAutomaticPing:     true,
		DisableNestedTransaction: true,
		SkipDefaultTransaction:   true,
	})
	if err != nil {
		t.Fatalf("open dry-run db: %v", err)
	}
	return db
}

func dryRunSQL(t *testing.T, db *gorm.DB, fc func(tx *gorm.DB) *gorm.DB) (string, []interface{}) {
	t.Helper()

	stmt := fc(db.Session(&gorm.Session{DryRun: true})).Statement
	return stmt.SQL.String(), stmt.Vars
}

func TestCreateNormalInsert(t *testing.T) {
	db := openDryRun(t, GormModeDM)

	sql, vars := dryRunSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		return tx.Create(&testUser{Name: testUserName, Age: 20})
	})

	if !strings.HasPrefix(sql, `INSERT INTO "test_users"`) {
		t.Fatalf("sql = %q, want INSERT INTO \"test_users\" prefix", sql)
	}
	if !strings.Contains(sql, "VALUES (?,?)") || !strings.HasSuffix(sql, ";") {
		t.Fatalf("sql = %q, want VALUES clause terminated by ;", sql)
	}
	if strings.Contains(sql, "SET IDENTITY_INSERT") {
		t.Fatalf("dry-run must not execute identity insert: %q", sql)
	}
	if len(vars) != 2 {
		t.Fatalf("vars = %v, want 2 bind values", vars)
	}
}

func TestCreateBatchInsert(t *testing.T) {
	db := openDryRun(t, GormModeDM)

	users := []testUser{{Name: "a", Age: 1}, {Name: "b", Age: 2}, {Name: "c", Age: 3}}
	sql, vars := dryRunSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		return tx.Create(&users)
	})

	if got := strings.Count(sql, "(?"); got != 3 {
		t.Fatalf("sql = %q, want 3 value tuples, got %d", sql, got)
	}
	if len(vars) != 6 {
		t.Fatalf("vars = %v, want 6 bind values", vars)
	}
}

func TestCreateMergeUpsert(t *testing.T) {
	db := openDryRun(t, GormModeDM)

	sql, _ := dryRunSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{colName, colAge}),
		}).Create(&testUser{Name: testUserName, Age: 20})
	})

	for _, want := range []string{
		"MERGE INTO",
		`AS "excluded"`,
		" USING (",
		" FROM DUAL",
		" WHEN MATCHED THEN UPDATE SET",
		" WHEN NOT MATCHED THEN INSERT",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("sql = %q, want it to contain %q", sql, want)
		}
	}
	// The auto-increment primary key must not appear in the INSERT column
	// list of the MERGE statement.
	if strings.Contains(sql, "INSERT (\"id\")") {
		t.Fatalf("sql = %q, identity column leaked into MERGE insert list", sql)
	}
}

// With DoUpdates but no conflict columns (and no UpdateAll), the MERGE
// statement cannot be built and must degrade to a plain INSERT. Note that
// UpdateAll does NOT degrade: gorm fills OnConflict.Columns with the primary
// key columns in ConvertToCreateValues.
func TestCreateMergeUpsertWithoutColumnsDegradesToInsert(t *testing.T) {
	db := openDryRun(t, GormModeDM)

	sql, _ := dryRunSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		return tx.Clauses(clause.OnConflict{
			DoUpdates: clause.AssignmentColumns([]string{colAge}),
		}).Create(&testUser{Name: testUserName, Age: 20})
	})

	if !strings.HasPrefix(sql, "INSERT INTO") {
		t.Fatalf("sql = %q, want plain INSERT (MERGE needs explicit conflict columns)", sql)
	}
}

func TestCreateMySQLUpsert(t *testing.T) {
	db := openDryRun(t, GormModeMySQL)

	sql, vars := dryRunSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		return tx.Clauses(clause.OnConflict{
			UpdateAll: true,
		}).Create(&testUser{Name: testUserName, Age: 20})
	})

	for _, want := range []string{
		"/*DMGORM-UPSERT*/DECLARE",
		"%TYPE := ?;",
		"DUP_VAL_ON_INDEX",
		"EXECUTE IMMEDIATE",
		"v_upsert_flag",
		"IDENT_CURRENT",
		"END;",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("sql = %q, want it to contain %q", sql, want)
		}
	}
	if len(vars) != 2 {
		t.Fatalf("vars = %v, want 2 bind values", vars)
	}
}

func TestCreatePostgresUpsert(t *testing.T) {
	db := openDryRun(t, GormModePostgres)

	sql, vars := dryRunSQL(t, db, func(tx *gorm.DB) *gorm.DB {
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "id"}},
			DoUpdates: clause.AssignmentColumns([]string{colAge}),
		}).Create(&testUser{Name: testUserName, Age: 20})
	})

	for _, want := range []string{
		"/*DMGORM-UPSERT*/DECLARE",
		"v_columns emp_tab := emp_tab( 'id' )",
		"multiset except v_columns",
		"DUP_VAL_ON_INDEX",
		"IDENT_CURRENT",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("sql = %q, want it to contain %q", sql, want)
		}
	}
	// Only the row values (name, age) are bound: the age assignment
	// references the row variable instead of a separate bind value.
	if len(vars) != 2 {
		t.Fatalf("vars = %v, want 2 bind values", vars)
	}
}

func TestUpsertWithoutUpdateActionsFallsBackToInsert(t *testing.T) {
	for _, mode := range []int{GormModeMySQL, GormModePostgres} {
		db := openDryRun(t, mode)

		sql, _ := dryRunSQL(t, db, func(tx *gorm.DB) *gorm.DB {
			return tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "id"}},
			}).Create(&testUser{Name: testUserName, Age: 20})
		})

		if !strings.HasPrefix(sql, "INSERT INTO") {
			t.Fatalf("mode %d: sql = %q, want plain INSERT", mode, sql)
		}
	}
}

// recordingConnPool records every executed statement. It backs Create tests
// that verify the identity-insert execution order without a server.
type recordingConnPool struct {
	execs []string
}

func (p *recordingConnPool) PrepareContext(_ context.Context, _ string) (*sql.Stmt, error) {
	return nil, errors.New("not implemented")
}

// execResult is a sql.Result whose LastInsertId succeeds (with 0), unlike
// driver.RowsAffected which rejects it.
type execResult struct{ rows int64 }

func (r execResult) LastInsertId() (int64, error) { return 0, nil }

func (r execResult) RowsAffected() (int64, error) { return r.rows, nil }

func (p *recordingConnPool) ExecContext(_ context.Context, query string, _ ...interface{}) (sql.Result, error) {
	p.execs = append(p.execs, query)
	return execResult{rows: 1}, nil
}

func (p *recordingConnPool) QueryContext(_ context.Context, _ string, _ ...interface{}) (*sql.Rows, error) {
	return nil, errors.New("not implemented")
}

func (p *recordingConnPool) QueryRowContext(_ context.Context, _ string, _ ...interface{}) *sql.Row {
	panic("not implemented")
}

// TestCreateIdentityInsertOrder guards the SET IDENTITY_INSERT lifecycle:
// ON must execute before the INSERT and OFF only after it. The official
// dialect defers the OFF statement to the end of the create callback.
func TestCreateIdentityInsertOrder(t *testing.T) {
	pool := &recordingConnPool{}
	db, err := gorm.Open(New(Config{Conn: pool}), &gorm.Config{
		SkipDefaultTransaction: true,
		DisableAutomaticPing:   true,
	})
	if err != nil {
		t.Fatalf("open db with fake pool: %v", err)
	}

	user := testUser{ID: 42, Name: testUserName, Age: 20}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	if len(pool.execs) != 3 {
		t.Fatalf("executed %d statements %q, want 3 (ON, INSERT, OFF)", len(pool.execs), pool.execs)
	}
	if !strings.Contains(pool.execs[0], "SET IDENTITY_INSERT") || !strings.Contains(pool.execs[0], "ON;") {
		t.Fatalf("first statement = %q, want SET IDENTITY_INSERT ... ON", pool.execs[0])
	}
	if !strings.HasPrefix(pool.execs[1], `INSERT INTO`) || !strings.Contains(pool.execs[1], `"id"`) {
		t.Fatalf("second statement = %q, want the INSERT including the id column", pool.execs[1])
	}
	if !strings.Contains(pool.execs[2], "SET IDENTITY_INSERT") || !strings.Contains(pool.execs[2], "OFF;") {
		t.Fatalf("third statement = %q, want SET IDENTITY_INSERT ... OFF", pool.execs[2])
	}
}

func TestSplitTableName(t *testing.T) {
	tests := []struct {
		in     string
		schema string
		table  string
	}{
		{in: tableUsers, schema: "", table: tableUsers},
		{in: "\"users\"", schema: "", table: tableUsers},
		{in: "sch.users", schema: "sch", table: tableUsers},
		{in: `"sch"."users"`, schema: "sch", table: tableUsers},
		{in: "a.b.c", schema: "", table: "a.b.c"},
	}
	for _, tt := range tests {
		schema, table := splitTableName(tt.in)
		if schema != tt.schema || table != tt.table {
			t.Errorf("splitTableName(%q) = (%q, %q), want (%q, %q)",
				tt.in, schema, table, tt.schema, tt.table)
		}
	}
}
