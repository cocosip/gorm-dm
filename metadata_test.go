package dm

import (
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

const (
	metadataMixedTarget    = "MiXeD.TaBlE"
	metadataMixedTable     = "TaBlE"
	metadataMixedName      = "MiXeD"
	metadataPositionLabel  = "KEY_POSITION"
	metadataPrimaryLabel   = "IS_PRIMARY"
	metadataNonUniqueLabel = "NON_UNIQUE"
	metadataTableLabel     = "TABLE_NAME"
	metadataIndexLabel     = "INDEX_NAME"
	metadataColumnLabel    = "COLUMN_NAME"
	metadataIndexAgeName   = "idx_age_name"
	metadataIndexNameAge   = "idx_name_age"
	metadataPrimaryIndex   = "pk_users"
	metadataCurrentSchema  = "OtherApp"
	metadataQualifiedTable = `"MiXeD"."TaBlE"`
	metadataSchemaLabel    = "current_schema"
)

func TestColumnTypesIdentity(t *testing.T) {
	for _, tt := range []struct {
		name        string
		identity    driver.Value
		primary     bool
		want, known bool
	}{
		{"identity primary key", int64(1), true, true, true},
		{"assigned primary key", int64(0), true, false, true},
		{"ordinary column", int64(0), false, false, true},
		{"identity without primary key", int64(1), false, true, true},
		{"unknown identity", nil, true, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			steps := userColumnSteps()
			steps[2].contains = []string{"TABS.SUBTYPE$ IN ('UTAB', 'STAB')", "BITAND(TABS.INFO3, 63) = 0", "THEN BITAND(COLS.INFO2, 1) ELSE NULL END AS IS_IDENTITY"}
			steps[1].rows.values[0][1] = tt.primary
			for i := range steps[2].rows.values {
				steps[2].rows.values[i][2] = tt.identity
			}
			// Even a column named id with an identity-looking default must use the catalog flag.
			steps[2].rows.values[0][1] = "'IDENTITY(1,1)'"
			db := openScriptedDB(t, GormModeDM, steps...).Table(catalogTable)
			columns, err := db.Migrator().ColumnTypes(&testUser{})
			if err != nil {
				t.Fatal(err)
			}
			for _, column := range columns {
				if got, known := column.AutoIncrement(); got != tt.want || known != tt.known {
					t.Errorf("%s AutoIncrement = (%v, %v), want (%v, %v)", column.Name(), got, known, tt.want, tt.known)
				}
			}
			for _, step := range steps {
				if !step.rows.closed {
					t.Error("metadata rows not closed")
				}
			}
		})
	}
}

func TestGetIndexesKeyOrder(t *testing.T) {
	// Table order is id, name, age; the second index deliberately reverses name/age.
	rows := &sqlRows{columns: []string{metadataTableLabel, metadataColumnLabel, metadataIndexLabel, metadataNonUniqueLabel, metadataPrimaryLabel, metadataPositionLabel}, values: [][]driver.Value{
		{testUserTable, colAge, metadataIndexAgeName, true, false, int64(1)},
		{testUserTable, colName, metadataIndexAgeName, true, false, int64(2)},
		{testUserTable, "body", "idx_fulltext", true, false, int64(1)},
		{testUserTable, colName, metadataIndexNameAge, false, false, int64(1)},
		{testUserTable, colAge, metadataIndexNameAge, false, false, int64(2)},
		{testUserTable, "id", metadataPrimaryIndex, false, true, int64(1)},
	}}
	db := openScriptedDB(t, GormModeDM, sqlStep{
		sql:      "SF_GET_INDEX_KEY_SEQ(INDS.KEYNUM, INDS.KEYINFO, COLS.COLID) AS KEY_POSITION",
		contains: []string{"SYSCONTEXTINDEXES", "1 AS KEY_POSITION", "ORDER BY INDEX_NAME, KEY_POSITION"},
		args:     []driver.Value{catalogSchema, testUserTable}, rows: rows,
	}).Table(catalogTable)
	indexes, err := db.Migrator().GetIndexes(&testUser{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{metadataIndexAgeName: {colAge, colName}, metadataIndexNameAge: {colName, colAge}, "idx_fulltext": {"body"}, metadataPrimaryIndex: {"id"}}
	if len(indexes) != len(want) {
		t.Fatalf("indexes = %v", indexes)
	}
	for _, index := range indexes {
		if !reflect.DeepEqual(index.Columns(), want[index.Name()]) {
			t.Errorf("%s columns = %v", index.Name(), index.Columns())
		}
		if unique, known := index.Unique(); !known || unique != (index.Name() == metadataPrimaryIndex || index.Name() == metadataIndexNameAge) {
			t.Errorf("%s uniqueness = (%v, %v)", index.Name(), unique, known)
		}
		if primary, known := index.PrimaryKey(); !known || primary != (index.Name() == metadataPrimaryIndex) {
			t.Errorf("%s primary = (%v, %v)", index.Name(), primary, known)
		}
	}
	if !rows.closed {
		t.Error("index rows not closed")
	}
}

func TestGetIndexesErrors(t *testing.T) {
	wantErr := errors.New("index metadata failed")
	for _, tt := range []struct {
		name     string
		rows     *sqlRows
		queryErr error
		want     string
	}{
		{"catalog query", nil, wantErr, wantErr.Error()},
		{"catalog scan", &sqlRows{columns: []string{metadataNonUniqueLabel}, values: [][]driver.Value{{"not-a-boolean"}}}, nil, "couldn't convert"},
		{"catalog iteration", &sqlRows{columns: []string{metadataIndexLabel}, values: [][]driver.Value{{"idx_partial"}}, err: wantErr}, nil, wantErr.Error()},
		{"close", &sqlRows{columns: []string{metadataIndexLabel}, closeErr: wantErr}, nil, wantErr.Error()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, GormModeDM, sqlStep{sql: "AS IS_PRIMARY", args: []driver.Value{catalogSchema, testUserTable}, rows: tt.rows, err: tt.queryErr}).Table(catalogTable)
			_, err := db.Migrator().GetIndexes(&testUser{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %s", err, tt.want)
			}
			if tt.rows != nil && !tt.rows.closed {
				t.Error("failed index rows not closed")
			}
		})
	}
}

func TestMetadataSchemaAndCase(t *testing.T) {
	for _, tt := range []struct {
		name, override, schema, table, rawTable string
		value                                   interface{}
		uppercase                               bool
	}{
		{"current schema model", "", metadataCurrentSchema, testUserTable, `"test_users"`, &testUser{}, false},
		{"current schema string", "", metadataCurrentSchema, "MixedTable", `"MixedTable"`, "MixedTable", false},
		{"qualified string", "", metadataMixedName, metadataMixedTable, metadataQualifiedTable, metadataMixedTarget, false},
		{"quoted string", "", metadataMixedName, metadataMixedTable, metadataQualifiedTable, metadataQualifiedTable, false},
		{"table override", metadataMixedTarget, metadataMixedName, metadataMixedTable, metadataQualifiedTable, &testUser{}, false},
		{"uppercase strategy", "", metadataCurrentSchema, "TEST_USERS", `"TEST_USERS"`, &testUser{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, indexes := range []bool{false, true} {
				var steps []sqlStep
				if tt.schema == metadataCurrentSchema {
					// The connection's current schema differs from its login user; no USER query is allowed.
					steps = append(steps, sqlStep{sql: "SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')", rows: &sqlRows{columns: []string{metadataSchemaLabel}, values: [][]driver.Value{{tt.schema}}}})
				}
				args := []driver.Value{tt.schema, tt.table}
				if indexes {
					steps = append(steps, sqlStep{sql: "ORDER BY INDEX_NAME, KEY_POSITION", args: args, rows: &sqlRows{
						columns: []string{metadataTableLabel, metadataColumnLabel, metadataIndexLabel, metadataNonUniqueLabel, metadataPrimaryLabel, metadataPositionLabel},
						values:  [][]driver.Value{{tt.table, metadataMixedName, "idx_MiXeD", true, false, int64(1)}},
					}})
				} else {
					steps = append(steps,
						sqlStep{sql: "SELECT * FROM " + tt.rawTable + " WHERE 1 = 0", rows: &sqlRows{columns: []string{metadataMixedName}}},
						sqlStep{sql: columnConsSQL, args: args, rows: &sqlRows{columns: []string{"name", "pk", "is_unique"}}},
						sqlStep{sql: columnDefsSQL, args: args, rows: &sqlRows{columns: []string{"name", "default", "identity"}, values: [][]driver.Value{{metadataMixedName, nil, int64(0)}}}},
					)
				}
				db := openScriptedDB(t, GormModeDM, steps...)
				if tt.uppercase {
					db.NamingStrategy = NamingStrategy{NamingStrategy: schema.NamingStrategy{}}
				}
				if tt.override != "" {
					db = db.Table(tt.override)
				}
				if indexes {
					got, err := db.Migrator().GetIndexes(tt.value)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) != 1 || got[0].Name() != "idx_MiXeD" || !reflect.DeepEqual(got[0].Columns(), []string{metadataMixedName}) {
						t.Fatalf("indexes = %v", got)
					}
				} else {
					got, err := db.Migrator().ColumnTypes(tt.value)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) != 1 || got[0].Name() != metadataMixedName {
						t.Fatalf("columns = %v", got)
					}
				}
			}
		})
	}
}

func TestMetadataSchemaErrors(t *testing.T) {
	wantErr := errors.New("schema metadata failed")
	for _, tt := range []struct {
		name     string
		rows     *sqlRows
		queryErr error
	}{
		{"schema query", nil, wantErr},
		{"schema scan", &sqlRows{columns: []string{metadataSchemaLabel}, values: [][]driver.Value{{nil}}}, nil},
		{"schema iteration", &sqlRows{columns: []string{metadataSchemaLabel}, err: wantErr}, nil},
		{"close", &sqlRows{columns: []string{metadataSchemaLabel}, values: [][]driver.Value{{catalogSchema}}, closeErr: wantErr}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, call := range []func(gorm.Migrator) error{
				func(m gorm.Migrator) error { _, err := m.ColumnTypes(&testUser{}); return err },
				func(m gorm.Migrator) error { _, err := m.GetIndexes(&testUser{}); return err },
			} {
				var rows *sqlRows
				if tt.rows != nil {
					copyRows := *tt.rows
					rows = &copyRows
				}
				db := openScriptedDB(t, GormModeDM, sqlStep{sql: schemaSQL, rows: rows, err: tt.queryErr})
				if err := call(db.Migrator()); err == nil {
					t.Fatal("missing schema error")
				}
				if rows != nil && !rows.closed {
					t.Error("schema rows not closed")
				}
			}
		})
	}
}
