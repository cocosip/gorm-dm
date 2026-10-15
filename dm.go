// Package dm provides a GORM v2 dialector for the DaMeng (达梦, DM8)
// database. It is a pure-Go rewrite of the official DaMeng gorm_v2_dialect
// built on the open-source database/sql driver
// gitee.com/chunanyong/dm, so no CGO is required.
//
// Usage:
//
//	import (
//		"github.com/cocosip/gorm-dm"
//		"gorm.io/gorm"
//	)
//
//	db, err := gorm.Open(dm.Open("dm://SYSDBA:SYSDBA001@localhost:5236?schema=TEST"))
//
// The dialect preserves the official configuration API. See README for
// conflict handling, identity backfill and migration error semantics.
package dm

import (
	"database/sql"
	"fmt"
	"strings"

	_ "gitee.com/chunanyong/dm" // registers the "dm" database/sql driver
	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

// GormMode selects the SQL generation strategy for upsert statements
// (clause.OnConflict). See Config.GormMode.
const (
	// GormModeDM generates DaMeng-native MERGE INTO statements. The conflict
	// columns must be specified for updates; otherwise they become plain
	// INSERTs. Targetless DoNothing uses a DM procedural block.
	GormModeDM = 0
	// GormModeMySQL emulates MySQL's ON DUPLICATE KEY UPDATE syntax with a
	// DaMeng PL/SQL block.
	GormModeMySQL = 1
	// GormModePostgres emulates PostgreSQL's ON CONFLICT DO UPDATE syntax
	// with a DaMeng PL/SQL block, including conflict column validation,
	// constraint names and WHERE conditions.
	GormModePostgres = 2
)

var (
	// CreateClauses are the clauses used by the create callback.
	CreateClauses = []string{"INSERT", "VALUES", "ON CONFLICT"}
	// QueryClauses are the clauses used by the query callback.
	QueryClauses = []string{}
	// UpdateClauses are the clauses used by the update callback.
	// DaMeng does not support ORDER BY in UPDATE statements.
	UpdateClauses = []string{"UPDATE", "SET", "WHERE", "LIMIT"}
	// DeleteClauses are the clauses used by the delete callback.
	// DaMeng does not support ORDER BY in DELETE statements.
	DeleteClauses = []string{"DELETE", "FROM", "WHERE", "LIMIT"}
)

// Config holds the dialector configuration.
type Config struct {
	// DriverName overrides the database/sql driver name. Defaults to "dm".
	DriverName string
	// DSN is the DaMeng connection string, e.g.
	// dm://user:password@host:port?schema=NAME&compatibleMode=mysql
	DSN string
	// Conn allows injecting an existing connection pool instead of opening
	// one from DSN.
	Conn gorm.ConnPool
	// DefaultStringSize is the default VARCHAR size applied to string fields
	// without a size tag. Defaults to 0, which leaves unindexed string
	// columns as CLOB.
	DefaultStringSize uint
	// GormMode selects the upsert strategy: GormModeDM (default),
	// GormModeMySQL or GormModePostgres. It does not set the server's
	// COMPATIBLE_MODE or the driver's compatibleMode DSN option. All
	// strategies generate DM SQL, not another database's native dialect.
	GormMode int
}

// Dialector implements gorm.Dialector for DaMeng.
type Dialector struct {
	*Config
}

// Open returns a DaMeng dialector for the given DSN.
func Open(dsn string) gorm.Dialector {
	return &Dialector{Config: &Config{DSN: dsn}}
}

// New returns a DaMeng dialector for the given config.
func New(config Config) gorm.Dialector {
	return &Dialector{Config: &config}
}

// Name returns the dialect name.
func (d Dialector) Name() string {
	return "dm"
}

// Initialize opens the connection pool and registers the DaMeng callbacks.
func (d Dialector) Initialize(db *gorm.DB) (err error) {
	if d.DriverName == "" {
		d.DriverName = "dm"
	}

	if d.Conn != nil {
		db.ConnPool = d.Conn
	} else {
		db.ConnPool, err = sql.Open(d.DriverName, d.DSN)
		if err != nil {
			return
		}
	}

	callbacks.RegisterDefaultCallbacks(db, &callbacks.Config{
		CreateClauses: CreateClauses,
		QueryClauses:  QueryClauses,
		UpdateClauses: UpdateClauses,
		DeleteClauses: DeleteClauses,
	})
	if err := db.Callback().Create().Replace("gorm:create", Create); err != nil {
		return err
	}

	return nil
}

// DefaultValueOf preserves known database defaults in mixed-value batches.
func (d Dialector) DefaultValueOf(field *schema.Field) clause.Expression {
	if field.DefaultValueInterface == nil && field.DefaultValue != "" && field.DefaultValue != "(-)" {
		return clause.Expr{SQL: field.DefaultValue}
	}
	// shortcut: unknown defaults retain NULL, supply a default expression for mixed batches.
	return clause.Expr{SQL: "NULL"}
}

// Migrator returns the DaMeng migrator, which reads metadata from the DaMeng
// system catalog (SYS.SYSOBJECTS etc.) instead of information_schema.
func (d Dialector) Migrator(db *gorm.DB) gorm.Migrator {
	return Migrator{
		Migrator: migrator.Migrator{
			Config: migrator.Config{
				DB:                          db,
				Dialector:                   d,
				CreateIndexAfterCreateTable: true,
			},
		},
		Dialector: d,
	}
}

// BindVarTo writes the DaMeng bind variable placeholder.
func (d Dialector) BindVarTo(writer clause.Writer, _ *gorm.Statement, _ interface{}) {
	_ = writer.WriteByte('?')
}

// QuoteTo quotes an identifier with double quotes, preserving its case.
// Segments separated by dots are quoted individually; backticks are treated
// like MySQL-style quotes and unquoted on write.
func (d Dialector) QuoteTo(writer clause.Writer, str string) {
	var (
		underQuoted, selfQuoted bool
		continuousBacktick      int8
		shiftDelimiter          int8
	)

	for _, v := range []byte(str) {
		switch v {
		case '"':
			continuousBacktick++
			if continuousBacktick == 2 {
				_, _ = writer.WriteString(`""`)
				continuousBacktick = 0
			}
		case '.':
			if continuousBacktick > 0 || !selfQuoted {
				shiftDelimiter = 0
				underQuoted = false
				continuousBacktick = 0
				_ = writer.WriteByte('"')
			}
			_ = writer.WriteByte(v)
			continue
		default:
			if shiftDelimiter-continuousBacktick <= 0 && !underQuoted {
				_ = writer.WriteByte('"')
				underQuoted = true
				if selfQuoted = continuousBacktick > 0; selfQuoted {
					continuousBacktick--
				}
			}

			for ; continuousBacktick > 0; continuousBacktick-- {
				_, _ = writer.WriteString(`""`)
			}

			_ = writer.WriteByte(v)
		}
		shiftDelimiter++
	}

	if continuousBacktick > 0 && !selfQuoted {
		_, _ = writer.WriteString(`""`)
	}
	_ = writer.WriteByte('"')
}

// Explain returns a SQL string with the bind variables inlined.
func (d Dialector) Explain(sql string, vars ...interface{}) string {
	return logger.ExplainSQL(sql, nil, `'`, vars...)
}

// DataTypeOf maps a GORM schema field to a DaMeng column type.
func (d Dialector) DataTypeOf(field *schema.Field) string {
	switch field.DataType {
	case schema.Bool:
		return "BIT"
	case schema.Int, schema.Uint:
		return d.dataTypeOfInt(field)
	case schema.Float:
		return d.dataTypeOfFloat(field)
	case schema.String:
		return d.dataTypeOfString(field)
	case schema.Time:
		return d.dataTypeOfTime(field)
	case schema.Bytes:
		return d.dataTypeOfBytes(field)
	default:
		return d.dataTypeOfCustom(field)
	}
}

func (d Dialector) dataTypeOfInt(field *schema.Field) string {
	sqlType := "BIGINT"
	switch {
	case field.Size <= 8:
		sqlType = "TINYINT"
	case field.Size <= 16:
		sqlType = "SMALLINT"
	case field.Size <= 32:
		sqlType = "INT"
	}
	if field.AutoIncrement {
		sqlType += " IDENTITY(1,1)"
	}
	return sqlType
}

func (d Dialector) dataTypeOfFloat(field *schema.Field) string {
	if field.Precision > 0 {
		return fmt.Sprintf("DECIMAL(%d, %d)", field.Precision, field.Scale)
	}
	return "DOUBLE"
}

func (d Dialector) dataTypeOfString(field *schema.Field) string {
	size := field.Size

	if size == 0 {
		if d.DefaultStringSize > 0 {
			size = int(d.DefaultStringSize)
		} else {
			hasIndex := field.TagSettings["INDEX"] != "" || field.TagSettings["UNIQUE"] != "" || field.TagSettings["UNIQUEINDEX"] != ""
			// TEXT-like columns cannot have a default value.
			if field.PrimaryKey || field.HasDefaultValue || hasIndex {
				size = 255
			}
		}
	}

	if size <= 0 || size >= 32768 {
		return "CLOB"
	}
	return fmt.Sprintf("VARCHAR(%d)", size)
}

func (d Dialector) dataTypeOfTime(field *schema.Field) string {
	if field.Precision > 0 {
		return fmt.Sprintf("TIMESTAMP(%d)", field.Precision)
	}
	return "TIMESTAMP"
}

func (d Dialector) dataTypeOfBytes(field *schema.Field) string {
	if field.Size > 0 && field.Size < 32768 {
		return fmt.Sprintf("VARBINARY(%d)", field.Size)
	}
	return "BLOB"
}

func (d Dialector) dataTypeOfCustom(field *schema.Field) string {
	sqlType := string(field.DataType)

	if field.AutoIncrement &&
		!strings.Contains(strings.ToLower(sqlType), " auto_increment") &&
		!strings.Contains(strings.ToLower(sqlType), " identity") {
		sqlType += " IDENTITY(1,1)"
	}

	return sqlType
}

// SavePoint creates a savepoint inside a transaction.
func (d Dialector) SavePoint(tx *gorm.DB, name string) error {
	return tx.Exec("SAVEPOINT " + name).Error
}

// RollbackTo rolls the transaction back to the named savepoint.
func (d Dialector) RollbackTo(tx *gorm.DB, name string) error {
	return tx.Exec("ROLLBACK TO SAVEPOINT " + name).Error
}
