package dm

import (
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

type databaseDefaultRow struct {
	Name  string    `gorm:"size:64"`
	Stamp time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

func TestCreateMixedDatabaseDefaults(t *testing.T) {
	const proceduralDefault = `"stamp"%TYPE := CURRENT_TIMESTAMP;`
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	conflict := clause.OnConflict{
		Columns:   []clause.Column{{Name: colName}},
		DoUpdates: clause.AssignmentColumns([]string{"stamp"}),
	}
	for _, tt := range []struct {
		name     string
		mode     int
		conflict *clause.OnConflict
		want     string
	}{
		{"insert", GormModeDM, nil, "VALUES (?,?),(?,CURRENT_TIMESTAMP)"},
		{"merge", GormModeDM, &conflict, "UNION ALL SELECT ?,CURRENT_TIMESTAMP FROM DUAL"},
		{"mysql", GormModeMySQL, &conflict, proceduralDefault},
		{"postgres", GormModePostgres, &conflict, proceduralDefault},
		{"do nothing", GormModeDM, &clause.OnConflict{DoNothing: true}, proceduralDefault},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := []databaseDefaultRow{{Name: "a", Stamp: stamp}, {Name: "b"}}
			db := openDryRun(t, tt.mode)
			if tt.conflict != nil {
				db = db.Clauses(*tt.conflict)
			}
			tx := db.Create(&rows)
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			if query := tx.Statement.SQL.String(); !strings.Contains(query, tt.want) {
				t.Fatalf("SQL = %q, want %q", query, tt.want)
			}
			wantVars := []interface{}{"a", stamp, "b"}
			if !reflect.DeepEqual(tx.Statement.Vars, wantVars) {
				t.Fatalf("vars = %v, want %v", tx.Statement.Vars, wantVars)
			}
			// Execute through database/sql too: the expression must never be bound.
			execDB := openScriptedDB(t, tt.mode, sqlStep{
				sql: tt.want, args: []driver.Value{"a", stamp, "b"}, result: sqlResult{affected: 2},
			})
			if tt.conflict != nil {
				execDB = execDB.Clauses(*tt.conflict)
			}
			if err := execDB.Create(&rows).Error; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCreateAllDatabaseDefaultsOmitted(t *testing.T) {
	db := openScriptedDB(t, GormModeDM, sqlStep{
		sql: `("name") VALUES (?),(?);`, args: []driver.Value{"a", "b"}, result: sqlResult{affected: 2},
	})
	if err := db.Create(&[]databaseDefaultRow{{Name: "a"}, {Name: "b"}}).Error; err != nil {
		t.Fatal(err)
	}
}

func TestDefaultValueOfWithoutKnownExpression(t *testing.T) {
	d := Dialector{Config: &Config{}}
	for _, field := range []*schema.Field{
		{AutoIncrement: true, HasDefaultValue: true},
		{HasDefaultValue: true, DefaultValue: "(-)"},
	} {
		db := openDryRun(t, GormModeDM)
		d.DefaultValueOf(field).Build(db.Statement)
		if got := db.Statement.SQL.String(); got != "NULL" {
			t.Fatalf("default for %#v = %q, want NULL", field, got)
		}
	}
}
