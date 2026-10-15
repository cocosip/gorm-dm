package dm

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

func migrationField(t *testing.T, db *gorm.DB, model interface{}, name string) *schema.Field {
	t.Helper()
	stmt := &gorm.Statement{DB: db}
	if err := stmt.Parse(model); err != nil {
		t.Fatal(err)
	}
	field := stmt.Schema.LookUpField(name)
	if field == nil {
		t.Fatalf("missing field %s", name)
	}
	return field
}

func TestMigrateColumnUniqueConstraint(t *testing.T) {
	type uniqueName struct {
		Name string `gorm:"size:64;unique"`
	}
	type ignoredName struct {
		Name string `gorm:"size:64;unique;-:migration"`
	}
	type primaryName struct {
		Name string `gorm:"size:64;primaryKey"`
	}
	ddlErr := errors.New("constraint DDL failed")
	add := `ADD CONSTRAINT "uni_test_users_name" UNIQUE ("name")`
	drop := `DROP CONSTRAINT "uni_test_users_name"`
	for _, tt := range []struct {
		name   string
		model  interface{}
		unique sql.NullBool
		length int64
		steps  []sqlStep
		err    error
	}{
		{"add unique", &uniqueName{}, sql.NullBool{Valid: true}, 64, []sqlStep{{sql: add}}, nil},
		{"drop", &testUser{}, sql.NullBool{Bool: true, Valid: true}, 64, []sqlStep{{sql: drop}}, nil},
		{"unchanged unique", &uniqueName{}, sql.NullBool{Bool: true, Valid: true}, 64, nil, nil},
		{"unchanged ordinary", &testUser{}, sql.NullBool{Valid: true}, 64, nil, nil},
		{"unknown", &uniqueName{}, sql.NullBool{}, 64, nil, nil},
		{"ignored", &ignoredName{}, sql.NullBool{Valid: true}, 32, nil, nil},
		{"primary key", &primaryName{}, sql.NullBool{Bool: true, Valid: true}, 64, nil, nil},
		{"alter then add", &uniqueName{}, sql.NullBool{Valid: true}, 32, []sqlStep{{sql: modifyNameSQL}, {sql: add}}, nil},
		{"add failure", &uniqueName{}, sql.NullBool{Valid: true}, 64, []sqlStep{{sql: add, err: ddlErr}}, ddlErr},
		{"drop failure", &testUser{}, sql.NullBool{Bool: true, Valid: true}, 64, []sqlStep{{sql: drop, err: ddlErr}}, ddlErr},
		{"alter failure", &uniqueName{}, sql.NullBool{Valid: true}, 32, []sqlStep{{sql: modifyNameSQL, err: ddlErr}}, ddlErr},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, GormModeDM, tt.steps...).Table(catalogTable)
			column := migrator.ColumnType{
				DataTypeValue: sql.NullString{String: dbTypeVarchar, Valid: true},
				LengthValue:   sql.NullInt64{Int64: tt.length, Valid: true},
				NullableValue: sql.NullBool{Bool: true, Valid: true},
				UniqueValue:   tt.unique,
			}
			err := db.Migrator().MigrateColumn(tt.model, migrationField(t, db, tt.model, "Name"), column)
			if !errors.Is(err, tt.err) {
				t.Fatalf("MigrateColumn = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestAutoMigrateUniqueConstraint(t *testing.T) {
	type uniqueName struct {
		Name string `gorm:"size:64;unique"`
	}
	steps := []sqlStep{countStep(countTableSQL, 1, catalogSchema, testUserTable)}
	steps = append(steps, userColumnSteps()...)
	steps = append(steps, sqlStep{sql: `ADD CONSTRAINT "uni_test_users_name" UNIQUE ("name")`})
	db := openScriptedDB(t, GormModeDM, steps...).Table(catalogTable)
	if err := db.AutoMigrate(&uniqueName{}); err != nil {
		t.Fatal(err)
	}
}

func TestAutoMigratePreservesCompositeUnique(t *testing.T) {
	type compositeName struct {
		Name string `gorm:"size:64"`
		Age  int32
	}
	steps := []sqlStep{countStep(countTableSQL, 1, catalogSchema, testUserTable)}
	columns := userColumnSteps()
	// The catalog must report only single-column unique constraints as Unique.
	columns[1].sql = "COUNT(*) OVER (PARTITION BY CONS.ID) = 1"
	steps = append(steps, columns...)
	db := openScriptedDB(t, GormModeDM, steps...).Table(catalogTable)
	if err := db.AutoMigrate(&compositeName{}); err != nil {
		t.Fatal(err)
	}
}

func TestBooleanDefaultsAcrossDDL(t *testing.T) {
	type enabled struct {
		Enabled bool `gorm:"default:true"`
	}
	type disabled struct {
		Enabled bool `gorm:"default:false"`
	}
	for _, tt := range []struct {
		name  string
		model interface{}
		value bool
		num   int
	}{
		{"true", &enabled{}, true, 1},
		{"false", &disabled{}, false, 0},
	} {
		for _, operation := range []string{"create", "add", "alter"} {
			t.Run(tt.name+"/"+operation, func(t *testing.T) {
				db := openScriptedDB(t, GormModeDM, sqlStep{sql: fmt.Sprintf(`"enabled" BIT DEFAULT %d`, tt.num)})
				field := migrationField(t, db, tt.model, "Enabled")
				var err error
				switch operation {
				case "create":
					err = db.Migrator().CreateTable(tt.model)
				case "add":
					err = db.Migrator().AddColumn(tt.model, "Enabled")
				case "alter":
					err = db.Migrator().AlterColumn(tt.model, "Enabled")
				}
				if err != nil {
					t.Fatal(err)
				}
				if field.DefaultValueInterface != tt.value {
					t.Fatalf("cached default changed to %#v", field.DefaultValueInterface)
				}
			})
		}
	}
}

func TestMigrateColumnBooleanDefault(t *testing.T) {
	type enabled struct {
		Enabled bool `gorm:"default:true"`
	}
	type disabled struct {
		Enabled bool `gorm:"default:false"`
	}
	for _, tt := range []struct {
		name    string
		model   interface{}
		current string
		steps   []sqlStep
	}{
		{"true unchanged", &enabled{}, "1", nil},
		{"false unchanged", &disabled{}, "0", nil},
		{"set true", &enabled{}, "0", []sqlStep{{sql: `MODIFY "enabled" BIT DEFAULT 1 NULL`}}},
		{"set false", &disabled{}, "1", []sqlStep{{sql: `MODIFY "enabled" BIT DEFAULT 0 NULL`}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := openScriptedDB(t, GormModeDM, tt.steps...)
			column := migrator.ColumnType{
				DataTypeValue:     sql.NullString{String: "BIT", Valid: true},
				NullableValue:     sql.NullBool{Bool: true, Valid: true},
				DefaultValueValue: sql.NullString{String: tt.current, Valid: true},
			}
			if err := db.Migrator().MigrateColumn(tt.model, migrationField(t, db, tt.model, "Enabled"), column); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMigrateColumnDecimalSize(t *testing.T) {
	type decimal struct {
		Amount float64 `gorm:"precision:10;scale:4"`
	}
	type integerDecimal struct {
		Amount float64 `gorm:"precision:10"`
	}
	type customDecimal struct {
		Amount float64 `gorm:"type:DECIMAL(10,4)"`
	}
	type customNumeric struct {
		Amount float64 `gorm:"type:NUMERIC(10,4)"`
	}
	type customInteger struct {
		Amount float64 `gorm:"type:NUMERIC(10)"`
	}
	type unspecified struct {
		Amount float64 `gorm:"type:DECIMAL"`
	}
	for _, tt := range []struct {
		name             string
		model            interface{}
		dataType         string
		precision, scale int64
		known            bool
		ddl              string
	}{
		{"scale changes", &decimal{}, dbTypeDecimal, 10, 2, true, "DECIMAL(10, 4)"},
		{"precision changes", &decimal{}, dbTypeDecimal, 8, 4, true, "DECIMAL(10, 4)"},
		{"both unchanged", &decimal{}, dbTypeDecimal, 10, 4, true, ""},
		{"scale zero", &integerDecimal{}, dbTypeDecimal, 10, 4, true, "DECIMAL(10, 0)"},
		{"scale zero unchanged", &integerDecimal{}, dbTypeDecimal, 10, 0, true, ""},
		{"custom decimal scale", &customDecimal{}, dbTypeDecimal, 10, 2, true, "DECIMAL(10,4)"},
		{"custom decimal precision", &customDecimal{}, dbTypeDecimal, 8, 4, true, "DECIMAL(10,4)"},
		{"custom decimal unchanged", &customDecimal{}, dbTypeDecimal, 10, 4, true, ""},
		{"custom numeric scale", &customNumeric{}, dbTypeNumeric, 10, 2, true, "NUMERIC(10,4)"},
		{"custom numeric unchanged", &customNumeric{}, dbTypeNumeric, 10, 4, true, ""},
		{"implicit zero scale", &customInteger{}, dbTypeNumeric, 10, 2, true, "NUMERIC(10)"},
		{"implicit zero unchanged", &customInteger{}, dbTypeNumeric, 10, 0, true, ""},
		{"unknown metadata", &decimal{}, dbTypeDecimal, 0, 0, false, ""},
		{"unspecified precision", &unspecified{}, dbTypeDecimal, 10, 2, true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var steps []sqlStep
			if tt.ddl != "" {
				steps = append(steps, sqlStep{sql: `MODIFY "amount" ` + tt.ddl})
			}
			db := openScriptedDB(t, GormModeDM, steps...)
			column := migrator.ColumnType{
				DataTypeValue:    sql.NullString{String: tt.dataType, Valid: true},
				NullableValue:    sql.NullBool{Bool: true, Valid: true},
				DecimalSizeValue: sql.NullInt64{Int64: tt.precision, Valid: tt.known},
				ScaleValue:       sql.NullInt64{Int64: tt.scale, Valid: tt.known},
			}
			if err := db.Migrator().MigrateColumn(tt.model, migrationField(t, db, tt.model, "Amount"), column); err != nil {
				t.Fatal(err)
			}
		})
	}
}
