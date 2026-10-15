package dm

import (
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	dmdrv "gitee.com/chunanyong/dm"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	insertPrefix     = "INSERT INTO"
	uniqueTargetName = "unique_name"
)

func TestCreateBackfillsGeneratedIDs(t *testing.T) {
	tests := []struct {
		name string
		rows []testUser
		id   int64
		want []uint64
	}{
		{"single", []testUser{{Name: "a", Age: 1}}, 41, []uint64{41}},
		{"batch", []testUser{{Name: "a", Age: 1}, {Name: "b", Age: 2}}, 42, []uint64{41, 42}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := make([]driver.Value, 0, len(tt.rows)*2)
			for _, row := range tt.rows {
				args = append(args, row.Name, int64(row.Age))
			}
			db := openScriptedDB(t, GormModeDM, sqlStep{
				sql: insertPrefix, args: args, result: sqlResult{id: tt.id, affected: int64(len(tt.rows))},
			})
			var tx *gorm.DB
			if len(tt.rows) == 1 {
				tx = db.Create(&tt.rows[0])
			} else {
				tx = db.Create(&tt.rows)
			}
			if tx.Error != nil || tx.RowsAffected != int64(len(tt.rows)) {
				t.Fatalf("Create: error=%v rows=%d", tx.Error, tx.RowsAffected)
			}
			for i, want := range tt.want {
				if tt.rows[i].ID != want {
					t.Errorf("row %d ID = %d, want %d", i, tt.rows[i].ID, want)
				}
			}
		})
	}
}

func TestMergeWithGeneratedPrimaryTargetUsesInsert(t *testing.T) {
	db := openDryRun(t, GormModeDM)
	tx := db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&testUser{Name: testUserName})
	if tx.Error != nil || !strings.HasPrefix(tx.Statement.SQL.String(), insertPrefix) {
		t.Fatalf("generated primary target: SQL=%s error=%v", tx.Statement.SQL.String(), tx.Error)
	}
}

func TestCreatePropagatesExecutionErrors(t *testing.T) {
	wantErr := errors.New("execution failed")
	tests := []struct {
		name string
		step sqlStep
	}{
		{"exec", sqlStep{err: wantErr}},
		{"affected rows", sqlStep{result: sqlResult{affectedErr: wantErr}}},
		{"last insert ID", sqlStep{result: sqlResult{affected: 1, idErr: wantErr}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.step.sql = insertPrefix
			tt.step.args = []driver.Value{testUserName, int64(20)}
			db := openScriptedDB(t, GormModeDM, tt.step)
			user := testUser{Name: testUserName, Age: 20}
			if err := db.Create(&user).Error; !errors.Is(err, wantErr) {
				t.Fatalf("Create error = %v, want %v", err, wantErr)
			}
			if user.ID != 0 {
				t.Fatalf("failed insert backfilled ID: %d", user.ID)
			}
		})
	}
}

func TestCreateIdentityInsertCleanupOnFailure(t *testing.T) {
	wantErr := errors.New("identity operation failed")
	identityOn := sqlStep{sql: `SET IDENTITY_INSERT "test_users" ON;`}
	insert := sqlStep{sql: insertPrefix, args: []driver.Value{testUserName, int64(20), int64(42)}}
	identityOff := sqlStep{sql: `SET IDENTITY_INSERT "test_users" OFF;`}
	tests := []struct {
		name  string
		steps []sqlStep
	}{
		{"enable", []sqlStep{{sql: identityOn.sql, err: wantErr}}},
		{"insert", []sqlStep{identityOn, {sql: insert.sql, args: insert.args, err: wantErr}, identityOff}},
		{"disable", []sqlStep{identityOn, insert, {sql: identityOff.sql, err: wantErr}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, GormModeDM, tt.steps...)
			user := testUser{ID: 42, Name: testUserName, Age: 20}
			if err := db.Create(&user).Error; !errors.Is(err, wantErr) {
				t.Fatalf("Create error = %v, want %v", err, wantErr)
			}
		})
	}
}

func TestCreateTableWithoutIdentityAcceptsExplicitID(t *testing.T) {
	db := openScriptedDB(t, GormModeDM,
		sqlStep{sql: "SET IDENTITY_INSERT", err: &dmdrv.DmError{ErrCode: errCodeNoIdentityColumn}},
		sqlStep{sql: insertPrefix, args: []driver.Value{testUserName, int64(20), int64(42)}, result: sqlResult{affected: 1}},
	)
	user := testUser{ID: 42, Name: testUserName, Age: 20}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.ID != 42 {
		t.Fatalf("explicit ID overwritten: %d", user.ID)
	}
}

// The -2717 answer is remembered per primary field: later inserts skip the
// SET IDENTITY_INSERT probe, and the cached schema's AutoIncrement flag stays
// untouched because concurrent inserts keep reading it.
func TestNoIdentityColumnIsCachedWithoutSchemaMutation(t *testing.T) {
	db := openScriptedDB(t, GormModeDM,
		sqlStep{sql: "SET IDENTITY_INSERT", err: &dmdrv.DmError{ErrCode: errCodeNoIdentityColumn}},
		sqlStep{sql: insertPrefix, args: []driver.Value{testUserName, int64(20), int64(42)}, result: sqlResult{affected: 1}},
		sqlStep{sql: insertPrefix, args: []driver.Value{testUserName, int64(20), int64(43)}, result: sqlResult{affected: 1}},
	)
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&testUser{}); err != nil {
		t.Fatal(err)
	}
	field := stmt.Schema.PrioritizedPrimaryField

	for i, id := range []uint64{42, 43} {
		user := testUser{ID: id, Name: testUserName, Age: 20}
		if err := db.Create(&user).Error; err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if user.ID != id {
			t.Fatalf("create %d: explicit ID overwritten: %d", i, user.ID)
		}
	}

	if !field.AutoIncrement {
		t.Fatal("AutoIncrement flag of the cached schema was mutated")
	}
}

func TestConflictBackfillsIDsThroughSuppliedKey(t *testing.T) {
	for _, mode := range []int{GormModeDM, GormModePostgres} {
		t.Run(map[int]string{GormModeDM: "merge", GormModePostgres: "postgres"}[mode], func(t *testing.T) {
			db := openScriptedDB(t, mode,
				sqlStep{sql: `"test_users"`, args: []driver.Value{testUserName, int64(20)}, result: sqlResult{affected: 1, idErr: errors.New("must not read identity")}},
				sqlStep{sql: `SELECT "id" FROM "test_users" WHERE "name" = ?`, args: []driver.Value{testUserName}, rows: &sqlRows{columns: []string{"id"}, values: [][]driver.Value{{int64(73)}}}},
			)
			user := testUser{Name: testUserName, Age: 20}
			tx := db.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: colName}}, DoUpdates: clause.AssignmentColumns([]string{colAge}),
			}).Create(&user)
			if tx.Error != nil || tx.RowsAffected != 1 || user.ID != 73 {
				t.Fatalf("upsert: error=%v rows=%d user=%+v", tx.Error, tx.RowsAffected, user)
			}
		})
	}
}

func TestConflictBackfillRejectsUnreliableResults(t *testing.T) {
	wantErr := errors.New("lookup failed")
	tests := []struct {
		name string
		rows *sqlRows
		err  error
		want string
	}{
		{"query", nil, wantErr, wantErr.Error()},
		{"scan", &sqlRows{columns: []string{"id"}, values: [][]driver.Value{{"invalid"}}}, nil, "converting"},
		{"multiple matches", &sqlRows{columns: []string{"id"}, values: [][]driver.Value{{int64(1)}, {int64(2)}}}, nil, "multiple rows"},
		{"iteration", &sqlRows{columns: []string{"id"}, err: wantErr}, nil, wantErr.Error()},
		{"no match", &sqlRows{columns: []string{"id"}}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, GormModeDM,
				sqlStep{sql: "MERGE INTO", args: []driver.Value{testUserName, int64(20)}, result: sqlResult{affected: 1}},
				sqlStep{sql: `SELECT "id"`, args: []driver.Value{testUserName}, rows: tt.rows, err: tt.err},
			)
			user := testUser{Name: testUserName, Age: 20}
			err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: colName}}, DoUpdates: clause.AssignmentColumns([]string{colAge})}).Create(&user).Error
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if user.ID != 0 {
				t.Fatalf("unreliable ID was backfilled: %d", user.ID)
			}
		})
	}
}

func TestConflictSkipsUnreliableIdentityBackfill(t *testing.T) {
	tests := []struct {
		name     string
		mode     int
		conflict clause.OnConflict
		rows     int64
	}{
		{"mysql count", GormModeMySQL, clause.OnConflict{UpdateAll: true}, 1},
		{"do nothing", GormModeDM, clause.OnConflict{DoNothing: true}, 0},
		{"changed key", GormModePostgres, clause.OnConflict{Columns: []clause.Column{{Name: colName}}, DoUpdates: clause.Assignments(map[string]interface{}{colName: "renamed"})}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []driver.Value{testUserName, int64(20)}
			if tt.name == "changed key" {
				args = append(args, "renamed")
			}
			db := openScriptedDB(t, tt.mode, sqlStep{sql: "DECLARE", args: args})
			user := testUser{Name: testUserName, Age: 20}
			tx := db.Clauses(tt.conflict).Create(&user)
			if tx.Error != nil || tx.RowsAffected != tt.rows || user.ID != 0 {
				t.Fatalf("upsert: error=%v rows=%d ID=%d", tx.Error, tx.RowsAffected, user.ID)
			}
		})
	}
}

func TestCreateRejectsUnsupportedConflictTargets(t *testing.T) {
	tests := []struct {
		name     string
		mode     int
		conflict clause.OnConflict
		want     string
	}{
		{"partial index", GormModePostgres, clause.OnConflict{TargetWhere: clause.Where{Exprs: []clause.Expression{clause.Eq{Column: colAge, Value: 20}}}}, "partial-index"},
		{"named merge", GormModeDM, clause.OnConflict{OnConstraint: uniqueTargetName}, "named conflict"},
		{"named mysql", GormModeMySQL, clause.OnConflict{OnConstraint: uniqueTargetName}, "named conflict"},
		{"missing merge value", GormModeDM, clause.OnConflict{Columns: []clause.Column{{Name: "missing"}}, DoNothing: true}, "no insert value"},
		{"missing update source", GormModePostgres, clause.OnConflict{DoUpdates: clause.AssignmentColumns([]string{"missing"})}, "no insert value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, tt.mode)
			err := db.Clauses(tt.conflict).Create(&testUser{Name: testUserName, Age: 20}).Error
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMergeUpdatePreservesPredicateAndSkipsTargetAssignments(t *testing.T) {
	db := openDryRun(t, GormModeDM)
	tx := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: colName}},
		DoUpdates: clause.AssignmentColumns([]string{colName, colAge}),
		Where:     clause.Where{Exprs: []clause.Expression{clause.Gt{Column: colAge, Value: 10}}},
	}).Create(&testUser{Name: testUserName, Age: 20})
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	query := tx.Statement.SQL.String()
	if strings.Contains(query, `SET "name"`) || !strings.Contains(query, `SET "age"="excluded"."age" WHERE "age" > ?`) {
		t.Fatalf("unexpected merge update: %s", query)
	}
	if !reflect.DeepEqual(tx.Statement.Vars, []interface{}{testUserName, int32(20), 10}) {
		t.Fatalf("vars = %v", tx.Statement.Vars)
	}
}

func TestUpsertSeparatelyBindsExpressionsAndPredicates(t *testing.T) {
	db := openDryRun(t, GormModePostgres)
	tx := db.Clauses(clause.OnConflict{
		OnConstraint: uniqueTargetName,
		DoUpdates:    clause.Assignments(map[string]interface{}{colAge: gorm.Expr(`"age" + ?`, 3)}),
		Where:        clause.Where{Exprs: []clause.Expression{clause.Lt{Column: colAge, Value: 100}}},
	}).Create(&[]testUser{{Name: "a", Age: 1}, {Name: "b", Age: 2}})
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	query := tx.Statement.SQL.String()
	for _, want := range []string{"CON_OBJ.NAME = 'unique_name'", ":dm_update_0_1", ":dm_update_1_1", ":dm_update_0_2", ":dm_update_1_2"} {
		if !strings.Contains(query, want) {
			t.Errorf("SQL missing %q: %s", want, query)
		}
	}
	want := []interface{}{"a", int32(1), "b", int32(2), 3, 100, 3, 100}
	if !reflect.DeepEqual(tx.Statement.Vars, want) {
		t.Fatalf("vars = %v, want %v", tx.Statement.Vars, want)
	}
}
