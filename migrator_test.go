package dm

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/migrator"
)

func TestGroupByIndexName(t *testing.T) {
	rows := []*Index{
		{TableName: "T1", IndexName: "IDX_A", ColumnName: "C1"},
		{TableName: "T1", IndexName: "IDX_A", ColumnName: "C2"},
		{TableName: "T1", IndexName: "IDX_B", ColumnName: "C3"},
	}

	grouped := groupByIndexName(rows)
	if len(grouped) != 2 {
		t.Fatalf("group count = %d, want 2", len(grouped))
	}
	if len(grouped["IDX_A"]) != 2 {
		t.Fatalf("IDX_A columns = %d, want 2", len(grouped["IDX_A"]))
	}
	if grouped["IDX_A"][0].ColumnName != "C1" || grouped["IDX_A"][1].ColumnName != "C2" {
		t.Fatalf("IDX_A column order = [%s %s], want [C1 C2]",
			grouped["IDX_A"][0].ColumnName, grouped["IDX_A"][1].ColumnName)
	}
}

const (
	catalogSchema = "APP"
	catalogTable  = "APP.test_users"
	testUserTable = "test_users"
	dbTypeInt     = "INT"
	dbTypeVarchar = "VARCHAR"
	dbTypeDecimal = "DECIMAL"
	dbTypeNumeric = "NUMERIC"
	modifyNameSQL = `MODIFY "name" VARCHAR(64) NULL`
	schemaSQL     = "SELECT SYS_CONTEXT"
	countTableSQL = "COUNT(TABS.NAME)"
	columnConsSQL = "COLS.NAME, LNNVL"
	columnDefsSQL = "COLS.NAME, COLS.DEFVAL"
)

func countStep(query string, count int64, args ...driver.Value) sqlStep {
	return sqlStep{sql: query, args: args, rows: &sqlRows{columns: []string{"count"}, values: [][]driver.Value{{count}}}}
}

func userColumnSteps() []sqlStep {
	return []sqlStep{
		{sql: `SELECT * FROM "APP"."test_users" WHERE 1 = 0`, rows: &sqlRows{columns: []string{"id", colName, colAge}, types: []string{"BIGINT", dbTypeVarchar, dbTypeInt}}},
		{sql: columnConsSQL, args: []driver.Value{catalogSchema, testUserTable}, rows: &sqlRows{columns: []string{colName, "pk", "unique"}, values: [][]driver.Value{{"id", true, false}, {colName, false, false}}}},
		{sql: columnDefsSQL, args: []driver.Value{catalogSchema, testUserTable}, rows: &sqlRows{columns: []string{colName, "default"}, values: [][]driver.Value{{"id", nil}, {colName, nil}, {colAge, nil}}}},
	}
}

func TestMigratorMetadataExistence(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		args []driver.Value
		call func(gorm.Migrator) bool
	}{
		{"table", countTableSQL, []driver.Value{catalogSchema, testUserTable}, func(m gorm.Migrator) bool { return m.HasTable(&testUser{}) }},
		{"column Go name", "COUNT(DISTINCT COLS.NAME)", []driver.Value{catalogSchema, testUserTable, colName}, func(m gorm.Migrator) bool { return m.HasColumn(&testUser{}, "Name") }},
		{"column table string", "COUNT(DISTINCT COLS.NAME)", []driver.Value{catalogSchema, testUserTable, colAge}, func(m gorm.Migrator) bool { return m.HasColumn(catalogTable, colAge) }},
		{"index", "COUNT(DISTINCT INDEX_NAME)", []driver.Value{catalogSchema, testUserTable, "idx_name", "idx_name"}, func(m gorm.Migrator) bool { return m.HasIndex(&testUser{}, "idx_name") }},
		{"constraint", "count(CON_OBJ.NAME)", []driver.Value{catalogSchema, "check_age", testUserTable}, func(m gorm.Migrator) bool { return m.HasConstraint(&testUser{}, "check_age") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, count := range []int64{0, 1} {
				db := openScriptedDB(t, GormModeDM, countStep(tt.sql, count, tt.args...)).Table(catalogTable)
				if got := tt.call(db.Migrator()); got != (count > 0) {
					t.Fatalf("count=%d: exists=%v", count, got)
				}
			}
		})
	}
}

func TestMigratorMetadataErrorsAreRecorded(t *testing.T) {
	wantErr := errors.New("catalog unavailable")
	tests := []struct {
		name string
		call func(gorm.Migrator) bool
	}{
		{"table lookup", func(m gorm.Migrator) bool { return m.HasTable(&testUser{}) }},
		{"column", func(m gorm.Migrator) bool { return m.HasColumn(&testUser{}, colName) }},
		{"index lookup", func(m gorm.Migrator) bool { return m.HasIndex(&testUser{}, "idx_name") }},
		{"constraint", func(m gorm.Migrator) bool { return m.HasConstraint(&testUser{}, "check_age") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, GormModeDM, sqlStep{sql: schemaSQL, err: wantErr})
			m := db.Migrator().(Migrator)
			if tt.call(m) || !errors.Is(m.DB.Error, wantErr) {
				t.Fatalf("catalog error not recorded: %v", m.DB.Error)
			}
		})
	}
}

func TestMigratorCurrentSchemaAndTables(t *testing.T) {
	db := openScriptedDB(t, GormModeDM,
		sqlStep{sql: schemaSQL, rows: &sqlRows{columns: []string{"schema"}, values: [][]driver.Value{{catalogSchema}}}},
		sqlStep{sql: schemaSQL, rows: &sqlRows{columns: []string{"schema"}, values: [][]driver.Value{{catalogSchema}}}},
		sqlStep{sql: "TABS.NAME FROM", args: []driver.Value{catalogSchema}, rows: &sqlRows{columns: []string{colName}, values: [][]driver.Value{{"users"}, {"orders"}}}},
	)
	m := db.Migrator()
	if got := m.CurrentDatabase(); got != catalogSchema {
		t.Fatalf("CurrentDatabase = %q", got)
	}
	tables, err := m.GetTables()
	if err != nil || !reflect.DeepEqual(tables, []string{"users", "orders"}) {
		t.Fatalf("GetTables = %v, %v", tables, err)
	}
}

func TestMigratorColumnTypes(t *testing.T) {
	steps := userColumnSteps()
	steps[1].rows.values[1][2] = true
	steps[2].rows.values[1][1] = "'guest'"
	db := openScriptedDB(t, GormModeDM, steps...).Table(catalogTable)
	columns, err := db.Migrator().ColumnTypes(&testUser{})
	if err != nil || len(columns) != 3 {
		t.Fatalf("ColumnTypes = %v, %v", columns, err)
	}
	if primary, ok := columns[0].PrimaryKey(); !ok || !primary {
		t.Fatal("primary key metadata missing")
	}
	if unique, ok := columns[1].Unique(); !ok || !unique {
		t.Fatal("unique metadata missing")
	}
	if dv, ok := columns[1].DefaultValue(); !ok || dv != "guest" {
		t.Fatalf("default = %q, %v", dv, ok)
	}
	if columns[2].Name() != colAge || columns[2].DatabaseTypeName() != dbTypeInt {
		t.Fatalf("driver metadata missing: %v", columns[2])
	}
	if unique, ok := columns[2].Unique(); !ok || unique {
		t.Fatalf("non-unique column = %v, known=%v", unique, ok)
	}
}

func TestMigratorColumnTypesRejectsMetadataErrors(t *testing.T) {
	wantErr := errors.New("metadata failed")
	tests := []struct {
		name string
		edit func([]sqlStep) []sqlStep
		want string
	}{
		{"driver query", func(s []sqlStep) []sqlStep { s[0].err = wantErr; return s[:1] }, wantErr.Error()},
		{"constraint query", func(s []sqlStep) []sqlStep { s[1].err = wantErr; return s[:2] }, wantErr.Error()},
		{"constraint scan", func(s []sqlStep) []sqlStep { s[1].rows.values[0][1] = "invalid"; return s[:2] }, "convert"},
		{"constraint iteration", func(s []sqlStep) []sqlStep { s[1].rows.err = wantErr; return s[:2] }, wantErr.Error()},
		{"column query", func(s []sqlStep) []sqlStep { s[2].err = wantErr; return s }, wantErr.Error()},
		{"column scan", func(s []sqlStep) []sqlStep { s[2].rows.columns = []string{colName}; return s }, "destination arguments"},
		{"column iteration", func(s []sqlStep) []sqlStep { s[2].rows.err = wantErr; return s }, wantErr.Error()},
		{"missing driver column", func(s []sqlStep) []sqlStep { s[2].rows.values[0][0] = "unknown"; return s }, "missing from driver"},
		{"incomplete catalog", func(s []sqlStep) []sqlStep { s[2].rows.values = s[2].rows.values[:1]; return s }, "incomplete catalog"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, GormModeDM, tt.edit(userColumnSteps())...).Table(catalogTable)
			_, err := db.Migrator().ColumnTypes(&testUser{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ColumnTypes error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestMigratorGetIndexes(t *testing.T) {
	db := openScriptedDB(t, GormModeDM, sqlStep{
		sql: "AS IS_PRIMARY", args: []driver.Value{catalogSchema, testUserTable},
		rows: &sqlRows{columns: []string{"TABLE_NAME", "COLUMN_NAME", "INDEX_NAME", "NON_UNIQUE", "IS_PRIMARY"}, values: [][]driver.Value{
			{testUserTable, "id", "pk_users", false, true},
			{testUserTable, colName, "idx_name_age", true, false},
			{testUserTable, colAge, "idx_name_age", true, false},
		}},
	}).Table(catalogTable)
	indexes, err := db.Migrator().GetIndexes(&testUser{})
	if err != nil || len(indexes) != 2 {
		t.Fatalf("GetIndexes = %v, %v", indexes, err)
	}
	for _, index := range indexes {
		if index.Name() == "pk_users" {
			primary, _ := index.PrimaryKey()
			unique, _ := index.Unique()
			if !primary || !unique {
				t.Fatal("primary index flags missing")
			}
		} else if !reflect.DeepEqual(index.Columns(), []string{colName, colAge}) {
			t.Fatalf("index columns = %v", index.Columns())
		}
	}
}

func TestAutoMigrateStopsBeforeDDLOnCatalogFailure(t *testing.T) {
	wantErr := errors.New("catalog denied")
	db := openScriptedDB(t, GormModeDM, sqlStep{sql: countTableSQL, args: []driver.Value{catalogSchema, testUserTable}, err: wantErr}).Table(catalogTable)
	if err := db.AutoMigrate(&testUser{}); !errors.Is(err, wantErr) {
		t.Fatalf("AutoMigrate error = %v", err)
	}
}

func TestAutoMigrateCreatesMissingTable(t *testing.T) {
	db := openScriptedDB(t, GormModeDM,
		countStep(countTableSQL, 0, catalogSchema, testUserTable),
		sqlStep{sql: `CREATE TABLE "APP"."test_users"`},
	).Table(catalogTable)
	if err := db.AutoMigrate(&testUser{}); err != nil {
		t.Fatal(err)
	}
}

func TestAutoMigrateAddsMissingColumn(t *testing.T) {
	steps := []sqlStep{countStep(countTableSQL, 1, catalogSchema, testUserTable)}
	columns := userColumnSteps()
	columns[0].rows.columns = columns[0].rows.columns[:2]
	columns[0].rows.types = columns[0].rows.types[:2]
	columns[2].rows.values = columns[2].rows.values[:2]
	steps = append(steps, columns...)
	steps = append(steps, sqlStep{sql: `ALTER TABLE "APP"."test_users" ADD "age" INT`})
	db := openScriptedDB(t, GormModeDM, steps...).Table(catalogTable)
	if err := db.AutoMigrate(&testUser{}); err != nil {
		t.Fatal(err)
	}
}

type migrationCommentModel struct {
	ID   uint64
	Name string `gorm:"size:64;comment:owner's name"`
}

func TestMigratorDDLAndComments(t *testing.T) {
	tests := []struct {
		name  string
		steps []sqlStep
		call  func(gorm.Migrator) error
	}{
		{"create table", []sqlStep{{sql: `CREATE TABLE "APP"."test_users"`}, {sql: `COMMENT ON COLUMN "APP"."test_users"."name" IS 'owner''s name'`}}, func(m gorm.Migrator) error { return m.CreateTable(&migrationCommentModel{}) }},
		{"add column", []sqlStep{{sql: `ADD "name" VARCHAR(64)`}, {sql: `COMMENT ON COLUMN`}}, func(m gorm.Migrator) error { return m.AddColumn(&migrationCommentModel{}, "Name") }},
		{"alter column", []sqlStep{{sql: modifyNameSQL}, {sql: `COMMENT ON COLUMN`}}, func(m gorm.Migrator) error { return m.AlterColumn(&migrationCommentModel{}, "Name") }},
		{"drop table", []sqlStep{{sql: `DROP TABLE IF EXISTS "APP"."test_users" CASCADE`}}, func(m gorm.Migrator) error { return m.DropTable(&testUser{}) }},
		{"rename table", []sqlStep{{sql: `ALTER TABLE "APP"."test_users" RENAME TO "renamed"`}}, func(m gorm.Migrator) error { return m.RenameTable(catalogTable, "renamed") }},
		{"drop column", []sqlStep{{sql: `DROP COLUMN "name"`}}, func(m gorm.Migrator) error { return m.DropColumn(&testUser{}, "Name") }},
		{"rename column", []sqlStep{{sql: `RENAME COLUMN "name" TO "renamed"`}}, func(m gorm.Migrator) error { return m.RenameColumn(&testUser{}, "Name", "renamed") }},
		{"drop constraint", []sqlStep{{sql: `DROP CONSTRAINT "check_age"`}}, func(m gorm.Migrator) error { return m.DropConstraint(&testUser{}, "check_age") }},
		{"drop index", []sqlStep{{sql: `DROP INDEX "APP"."idx_name"`}}, func(m gorm.Migrator) error { return m.DropIndex(&testUser{}, "idx_name") }},
		{"rename index", []sqlStep{{sql: `ALTER INDEX "APP"."idx_name" RENAME TO "renamed"`}}, func(m gorm.Migrator) error { return m.RenameIndex(&testUser{}, "idx_name", "renamed") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, GormModeDM, tt.steps...).Table(catalogTable)
			if err := tt.call(db.Migrator()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrateColumnRelaxesNotNull(t *testing.T) {
	db := openScriptedDB(t, GormModeDM,
		sqlStep{sql: modifyNameSQL},
	).Table(catalogTable)
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(&testUser{}); err != nil {
		t.Fatal(err)
	}
	column := migrator.ColumnType{
		DataTypeValue: sql.NullString{String: dbTypeVarchar, Valid: true},
		LengthValue:   sql.NullInt64{Int64: 64, Valid: true},
		NullableValue: sql.NullBool{Bool: false, Valid: true},
	}
	if err := db.Migrator().MigrateColumn(&testUser{}, stmt.Schema.LookUpField("Name"), column); err != nil {
		t.Fatal(err)
	}
}

func TestMigratorViewsAndSavepoints(t *testing.T) {
	db := openScriptedDB(t, GormModeDM,
		sqlStep{sql: `CREATE VIEW "adult_users" AS SELECT "name" FROM "test_users" WHERE age >= 18`},
		sqlStep{sql: `DROP VIEW IF EXISTS "adult_users"`},
		sqlStep{sql: "SAVEPOINT before_update"},
		sqlStep{sql: "ROLLBACK TO SAVEPOINT before_update"},
	)
	query := db.Model(&testUser{}).Select(colName).Where("age >= ?", 18)
	if err := db.Migrator().CreateView("adult_users", gorm.ViewOption{Query: query}); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropView("adult_users"); err != nil {
		t.Fatal(err)
	}
	d := db.Dialector.(*Dialector)
	if err := d.SavePoint(db, "before_update"); err != nil {
		t.Fatal(err)
	}
	if err := d.RollbackTo(db, "before_update"); err != nil {
		t.Fatal(err)
	}
}

func TestMigratorCreatesConstraintsAndIndexes(t *testing.T) {
	type constrained struct {
		ID   uint64
		Name string `gorm:"size:64;index:idx_name"`
		Age  int32  `gorm:"check:check_age,age >= 0"`
	}
	db := openScriptedDB(t, GormModeDM,
		sqlStep{sql: `ADD CONSTRAINT "check_age" CHECK (age >= 0)`},
		sqlStep{sql: `CREATE INDEX "idx_name" ON "APP"."test_users"("name")`},
	).Table(catalogTable)
	if err := db.Migrator().CreateConstraint(&constrained{}, "check_age"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().CreateIndex(&constrained{}, "idx_name"); err != nil {
		t.Fatal(err)
	}
}
