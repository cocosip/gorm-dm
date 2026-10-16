package dm

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Raw DDL is intentional: expected metadata must not come from GORM model tags.
func TestIntegrationReadOnlyMetadata(t *testing.T) {
	db := integrationDB(t, GormModeDM)
	suffix := fmt.Sprint(time.Now().UnixNano())
	table, assigned, view := "DmMeta"+suffix, "DmAssigned"+suffix, "DmView"+suffix
	indexAB, indexBA, fulltext := "DmAB"+suffix, "DmBA"+suffix, "DmText"+suffix
	exec := func(query string, args ...interface{}) {
		t.Helper()
		if err := db.Exec(query, args...).Error; err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE ? ("Generated" BIGINT IDENTITY(7,3) PRIMARY KEY, "A" INT, "B" INT, "Body" CLOB)`, clause.Table{Name: table})
	t.Cleanup(func() {
		if err := db.Migrator().DropTable(table); err != nil {
			t.Error(err)
		}
	})
	exec(`CREATE TABLE ? ("ID" BIGINT PRIMARY KEY, "Ordinary" INT)`, clause.Table{Name: assigned})
	t.Cleanup(func() {
		if err := db.Migrator().DropTable(assigned); err != nil {
			t.Error(err)
		}
	})
	exec(`CREATE INDEX ? ON ? ("A", "B" DESC)`, clause.Table{Name: indexAB}, clause.Table{Name: table})
	exec(`CREATE UNIQUE INDEX ? ON ? ("B", "A")`, clause.Table{Name: indexBA}, clause.Table{Name: table})
	exec(`CREATE CONTEXT INDEX ? ON ? ("Body") LEXER DEFAULT_LEXER`, clause.Table{Name: fulltext}, clause.Table{Name: table})
	exec(`CREATE VIEW ? AS SELECT "Generated" FROM ?`, clause.Table{Name: view}, clause.Table{Name: table})
	t.Cleanup(func() {
		if err := db.Migrator().DropView(view); err != nil {
			t.Error(err)
		}
	})

	var currentSchema, loginUser string
	if err := db.Raw("SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA')").Row().Scan(&currentSchema); err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT USER").Row().Scan(&loginUser); err != nil {
		t.Fatal(err)
	}
	t.Run("current schema differs from login", func(t *testing.T) {
		if currentSchema == loginUser {
			t.Skip("set DM_TEST_DSN schema to an existing schema different from the login user to verify this case")
		}
		t.Logf("inspecting schema %q as user %q", currentSchema, loginUser)
	})
	for _, target := range []string{table, currentSchema + "." + table} {
		t.Run(target, func(t *testing.T) {
			checkIntegrationMetadata(t, db, target, map[string][]string{indexAB: {"A", "B"}, indexBA: {"B", "A"}, fulltext: {"Body"}}, indexBA)
		})
	}
	columns, err := db.Migrator().ColumnTypes(assigned)
	if err != nil {
		t.Fatal(err)
	}
	if len(columns) != 2 {
		t.Fatalf("assigned columns = %v", columns)
	}
	for _, column := range columns {
		if identity, known := column.AutoIncrement(); !known || identity {
			t.Errorf("%s assigned identity = (%v, %v)", column.Name(), identity, known)
		}
	}
	columns, err = db.Migrator().ColumnTypes(view)
	if err != nil {
		t.Fatal(err)
	}
	if len(columns) != 1 {
		t.Fatalf("view columns = %v", columns)
	}
	if identity, known := columns[0].AutoIncrement(); identity || known {
		t.Errorf("view identity = (%v, %v), want unknown", identity, known)
	}
}

func checkIntegrationMetadata(t *testing.T, db *gorm.DB, target string, want map[string][]string, uniqueName string) {
	t.Helper()
	columns, err := db.Migrator().ColumnTypes(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(columns) != 4 {
		t.Fatalf("columns = %v", columns)
	}
	for _, column := range columns {
		if identity, known := column.AutoIncrement(); !known || identity != (column.Name() == "Generated") {
			t.Errorf("%s identity = (%v, %v)", column.Name(), identity, known)
		}
	}
	indexes, err := db.Migrator().GetIndexes(target)
	if err != nil {
		t.Fatal(err)
	}
	primaryFound := false
	for _, index := range indexes {
		primary, primaryKnown := index.PrimaryKey()
		unique, uniqueKnown := index.Unique()
		if !primaryKnown || !uniqueKnown {
			t.Fatalf("%s missing index flags", index.Name())
		}
		if primary {
			primaryFound = true
			if !unique || !reflect.DeepEqual(index.Columns(), []string{"Generated"}) {
				t.Errorf("primary index = %v", index)
			}
		}
		if expected, ok := want[index.Name()]; ok {
			if primary || unique != (index.Name() == uniqueName) || !reflect.DeepEqual(index.Columns(), expected) {
				t.Errorf("%s index = %v", index.Name(), index)
			}
			delete(want, index.Name())
		}
	}
	if !primaryFound || len(want) != 0 {
		t.Fatalf("missing primary=%v or indexes=%v", !primaryFound, want)
	}
}
