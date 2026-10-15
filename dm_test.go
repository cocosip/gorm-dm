package dm

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm/schema"
)

// Test cases carried over from the official DaMeng dialect.
func TestDialectorQuoteTo(t *testing.T) {
	testdatas := []struct {
		raw    string
		expect string
	}{
		{"datadase.tableUser", `"datadase"."tableUser"`},
		{"datadase.table`User", "\"datadase\".\"table`User\""},
		{"`a`.`b`", "\"`a`\".\"`b`\""},
		{"`a`.b`", "\"`a`\".\"b`\""},
		{"a.`b`", "\"a\".\"`b`\""},
		{"`a`.b`c", "\"`a`\".\"b`c\""},
		{"`a`.`b`c`", "\"`a`\".\"`b`c`\""},
		{"`a`.b", "\"`a`\".\"b\""},
		{"`ab`", "\"`ab`\""},
		{"`a``b`", "\"`a``b`\""},
		{"`a```b`", "\"`a```b`\""},
		{"a`b", "\"a`b\""},
		{"ab", "\"ab\""},
		{"`a.b`", "\"`a\".\"b`\""},
		{"a.b", "\"a\".\"b\""},
	}

	dialector := Open("")
	for _, item := range testdatas {
		buf := &bytes.Buffer{}
		dialector.QuoteTo(buf, item.raw)
		if buf.String() != item.expect {
			t.Errorf("quote %q fail, got %q, expect %q", item.raw, buf.String(), item.expect)
		}
	}
}

func BenchmarkDialectorQuoteTo(b *testing.B) {
	dialector := Open("")
	buf := &bytes.Buffer{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dialector.QuoteTo(buf, "datadase.table`User")
		buf.Reset()
	}
}

func TestOpenAndNew(t *testing.T) {
	if got := Open("dm://u:p@h:1").Name(); got != "dm" {
		t.Fatalf("Open().Name() = %q, want %q", got, "dm")
	}
	if got := New(Config{DSN: "x"}).Name(); got != "dm" {
		t.Fatalf("New(Config{}).Name() = %q, want %q", got, "dm")
	}
}

func TestGormModeConstants(t *testing.T) {
	tests := []struct {
		mode  int
		value int
	}{
		{GormModeDM, 0},
		{GormModeMySQL, 1},
		{GormModePostgres, 2},
	}
	for _, tt := range tests {
		if tt.mode != tt.value {
			t.Errorf("mode constant = %d, want %d", tt.mode, tt.value)
		}
	}
}

type allTypesModel struct {
	ID             uint64 `gorm:"primaryKey;autoIncrement"`
	Bool           bool
	SmallInt       int16
	Int            int32
	BigInt         int64
	UInt           uint32
	Float          float64
	Decimal        float64 `gorm:"precision:10;scale:2"`
	Str            string
	StrSized       string `gorm:"size:100"`
	StrKey         string `gorm:"primaryKey"`
	StrIndexed     string `gorm:"index"`
	StrUnique      string `gorm:"uniqueIndex"`
	StrNamed       string `gorm:"uniqueIndex:idx_named"`
	StrPartA       string `gorm:"uniqueIndex:idx_pair"`
	StrPartB       string `gorm:"uniqueIndex:idx_pair"`
	StrUniqueSized string `gorm:"size:80;uniqueIndex"`
	Time           time.Time
	TimePrecise    time.Time `gorm:"precision:6"`
	Bytes          []byte
	BytesSized     []byte `gorm:"size:100"`
	CustomType     string `gorm:"type:MYTYPE"`
}

func TestDataTypeOf(t *testing.T) {
	const indexedStringType = "VARCHAR(255)"
	parsed, err := schema.Parse(&allTypesModel{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	d := Dialector{Config: &Config{}}
	tests := []struct {
		field string
		want  string
	}{
		{"ID", "BIGINT IDENTITY(1,1)"},
		{"Bool", "BIT"},
		{"SmallInt", "SMALLINT"},
		{"Int", "INT"},
		{"BigInt", "BIGINT"},
		{"UInt", "INT"},
		{"Float", "DOUBLE"},
		{"Decimal", dbTypeDecimalSized},
		{"Str", "CLOB"},
		{"StrSized", "VARCHAR(100)"},
		{"StrKey", indexedStringType},
		{"StrIndexed", indexedStringType},
		{"StrUnique", indexedStringType},
		{"StrNamed", indexedStringType},
		{"StrPartA", indexedStringType},
		{"StrPartB", indexedStringType},
		{"StrUniqueSized", "VARCHAR(80)"},
		{"Time", "TIMESTAMP"},
		{"TimePrecise", dbTypeTimestampSized},
		{"Bytes", "BLOB"},
		{"BytesSized", "VARBINARY(100)"},
		{"CustomType", "MYTYPE"},
	}
	for _, tt := range tests {
		field := parsed.LookUpField(tt.field)
		if field == nil {
			t.Fatalf("field %s not found", tt.field)
		}
		if got := d.DataTypeOf(field); got != tt.want {
			t.Errorf("DataTypeOf(%s) = %q, want %q", tt.field, got, tt.want)
		}
	}
}

func TestDataTypeOfDefaultStringSize(t *testing.T) {
	parsed, err := schema.Parse(&allTypesModel{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}

	d := Dialector{Config: &Config{DefaultStringSize: 256}}
	for _, name := range []string{"Str", "StrUnique", "StrNamed", "StrPartA", "StrPartB"} {
		if got := d.DataTypeOf(parsed.LookUpField(name)); got != "VARCHAR(256)" {
			t.Errorf("DataTypeOf(%s) = %q, want VARCHAR(256)", name, got)
		}
	}
}

func TestCreateTableUniqueIndexWithoutSize(t *testing.T) {
	type indexed struct {
		Name string `gorm:"uniqueIndex:idx_name"`
	}
	db := openScriptedDB(t, GormModeDM,
		sqlStep{sql: `CREATE TABLE "APP"."test_users" ("name" VARCHAR(255))`},
		sqlStep{sql: `CREATE UNIQUE INDEX "idx_name" ON "APP"."test_users"("name")`},
	).Table(catalogTable)
	if err := db.Migrator().CreateTable(&indexed{}); err != nil {
		t.Fatal(err)
	}
}

func TestExplain(t *testing.T) {
	d := Dialector{Config: &Config{}}
	got := d.Explain("SELECT * FROM t WHERE name = ?", "it's")
	want := "SELECT * FROM t WHERE name = 'it''s'"
	if got != want {
		t.Fatalf("Explain() = %q, want %q", got, want)
	}
}
