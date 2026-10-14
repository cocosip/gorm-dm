package dm

import (
	"strings"

	"gorm.io/gorm/schema"
)

// NamingStrategy is an optional Namer that uppercases the identifiers GORM
// generates while keeping the snake_case convention.
//
// DaMeng folds unquoted identifiers to uppercase (CASE_SENSITIVE=y default),
// so applications that mix GORM with raw SQL written against unquoted
// identifiers (e.g. casbin's default table definitions) should install this
// strategy to keep both worlds consistent:
//
//	gorm.Open(dm.Open(dsn), &gorm.Config{
//	    NamingStrategy: dm.NamingStrategy{
//	        NamingStrategy: schema.NamingStrategy{IdentifierMaxLength: 64},
//	    },
//	})
//
// It is not enabled by default: the dialect itself quotes identifiers and
// preserves their case, matching the official DaMeng dialect.
type NamingStrategy struct {
	schema.NamingStrategy
}

// TableName uppercases the snake_case table name.
func (ns NamingStrategy) TableName(table string) string {
	return strings.ToUpper(ns.NamingStrategy.TableName(table))
}

// SchemaName uppercases the schema name.
func (ns NamingStrategy) SchemaName(table string) string {
	return strings.ToUpper(ns.NamingStrategy.SchemaName(table))
}

// ColumnName uppercases the snake_case column name.
func (ns NamingStrategy) ColumnName(table, column string) string {
	return strings.ToUpper(ns.NamingStrategy.ColumnName(table, column))
}

// JoinTableName uppercases the snake_case join table name.
func (ns NamingStrategy) JoinTableName(joinTable string) string {
	return strings.ToUpper(ns.NamingStrategy.JoinTableName(joinTable))
}

// RelationshipFKName uppercases the foreign key constraint name.
func (ns NamingStrategy) RelationshipFKName(rel schema.Relationship) string {
	return strings.ToUpper(ns.NamingStrategy.RelationshipFKName(rel))
}

// CheckerName uppercases the check constraint name.
func (ns NamingStrategy) CheckerName(table, column string) string {
	return strings.ToUpper(ns.NamingStrategy.CheckerName(table, column))
}

// IndexName uppercases the index name.
func (ns NamingStrategy) IndexName(table, column string) string {
	return strings.ToUpper(ns.NamingStrategy.IndexName(table, column))
}

// UniqueName uppercases the unique constraint name.
func (ns NamingStrategy) UniqueName(table, column string) string {
	return strings.ToUpper(ns.NamingStrategy.UniqueName(table, column))
}
