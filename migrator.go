package dm

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/migrator"
	"gorm.io/gorm/schema"
)

var regFullDataType = regexp.MustCompile(`\D*(\d+)\D?`)

// Migrator is the DaMeng GORM migrator. It reads metadata from the DaMeng
// system catalog (SYS.SYSOBJECTS, SYS.SYSCOLUMNS, SYS.SYSCONS,
// SYS.SYSINDEXES) instead of information_schema. Server compatibility modes
// still require integration verification.
type Migrator struct {
	migrator.Migrator
	Dialector
}

// AutoMigrate keeps catalog errors separate from an absent object. The GORM
// base implementation only receives bool from HasTable/HasIndex/HasConstraint.
func (m Migrator) AutoMigrate(dst ...interface{}) error {
	for _, value := range m.ReorderModels(dst, true) {
		queryDB, execDB := m.GetQueryAndExecTx()
		query, exec := m.withDB(queryDB), m.withDB(execDB)
		exists, err := query.hasTable(value)
		if err != nil {
			return err
		}
		if !exists {
			if err = exec.CreateTable(value); err != nil {
				return err
			}
			continue
		}
		if err = m.RunWithValue(value, func(stmt *gorm.Statement) error {
			if stmt.Schema == nil {
				return fmt.Errorf("dm: AutoMigrate requires a model, got %T", value)
			}
			columns, err := query.ColumnTypes(value)
			if err != nil {
				return err
			}
			for _, name := range stmt.Schema.DBNames {
				var existing gorm.ColumnType
				for _, column := range columns {
					if column.Name() == name {
						existing = column
						break
					}
				}
				if existing == nil {
					if err = exec.AddColumn(value, name); err != nil {
						return err
					}
				} else if err = exec.MigrateColumn(value, stmt.Schema.FieldsByDBName[name], existing); err != nil {
					return err
				}
			}
			ensureConstraint := func(name string) error {
				exists, err := query.hasConstraint(value, name)
				if err != nil {
					return err
				}
				if !exists {
					return exec.CreateConstraint(value, name)
				}
				return nil
			}
			if !m.DB.DisableForeignKeyConstraintWhenMigrating && !m.DB.IgnoreRelationshipsWhenMigrating {
				for _, rel := range stmt.Schema.Relationships.Relations {
					if rel.Field.IgnoreMigration {
						continue
					}
					if constraint := rel.ParseConstraint(); constraint != nil && constraint.Schema == stmt.Schema {
						if err = ensureConstraint(constraint.Name); err != nil {
							return err
						}
					}
				}
			}
			for _, check := range stmt.Schema.ParseCheckConstraints() {
				if err = ensureConstraint(check.Name); err != nil {
					return err
				}
			}
			for _, index := range stmt.Schema.ParseIndexes() {
				exists, err := query.hasIndex(value, index.Name)
				if err != nil {
					return err
				}
				if !exists {
					if err = exec.CreateIndex(value, index.Name); err != nil {
						return err
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (m Migrator) withDB(db *gorm.DB) Migrator {
	m.DB = db
	return m
}

// CurrentDatabase returns the current schema and records lookup errors.
func (m Migrator) CurrentDatabase() string {
	name, err := m.currentDatabase()
	_ = m.DB.AddError(err)
	return name
}

func (m Migrator) currentDatabase() (string, error) {
	var name string
	err := m.DB.Raw("SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA');").Row().Scan(&name)
	return name, err
}

func (m Migrator) catalogTarget(stmt *gorm.Statement) (schemaName, table string, err error) {
	schemaName, table = splitTableName(getTableName(stmt))
	if schemaName == "" {
		schemaName, err = m.currentDatabase()
	}
	return
}

// FullDataTypeOf returns the full data type expression of a field.
func (m Migrator) FullDataTypeOf(field *schema.Field) clause.Expr {
	return m.Migrator.FullDataTypeOf(field)
}

// GetTypeAliases returns the type aliases of a database type.
func (m Migrator) GetTypeAliases(databaseTypeName string) []string {
	return m.Migrator.GetTypeAliases(databaseTypeName)
}

// CreateTable creates the tables and then attaches column comments, since
// DaMeng has no inline COMMENT syntax in CREATE TABLE. Boolean default
// values are rewritten to 1/0 because DaMeng rejects TRUE/FALSE literals.
func (m Migrator) CreateTable(values ...interface{}) error {
	for _, value := range values {
		if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
			for _, v := range stmt.Schema.Fields {
				if !v.HasDefaultValue {
					continue
				}
				if vv, ok := v.DefaultValueInterface.(bool); ok {
					if vv {
						v.DefaultValueInterface = int64(1)
					} else {
						v.DefaultValueInterface = int64(0)
					}
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := m.Migrator.CreateTable(values...); err != nil {
		return err
	}
	for _, value := range values {
		if err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
			for _, field := range stmt.Schema.FieldsByDBName {
				if err := m.alterColumnComment(stmt, field); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// DropTable drops the tables if they exist, children first.
func (m Migrator) DropTable(values ...interface{}) error {
	values = m.ReorderModels(values, false)
	for i := len(values) - 1; i >= 0; i-- {
		tx := m.DB.Session(&gorm.Session{})
		if err := m.RunWithValue(values[i], func(stmt *gorm.Statement) error {
			return tx.Exec("DROP TABLE IF EXISTS ? CASCADE", m.CurrentTable(stmt)).Error
		}); err != nil {
			return err
		}
	}
	return nil
}

// HasTable reports whether the table exists, honouring privileges.
func (m Migrator) HasTable(value interface{}) bool {
	exists, err := m.hasTable(value)
	_ = m.DB.AddError(err)
	return exists
}

func (m Migrator) hasTable(value interface{}) (bool, error) {
	const tableSQL = `SELECT /*+ MAX_OPT_N_TABLES(5) */ COUNT(TABS.NAME) FROM
(SELECT ID, PID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = ?) SCHEMAS,
(SELECT ID, SCHID, NAME FROM SYS.SYSOBJECTS WHERE
NAME = ? AND TYPE$ = 'SCHOBJ' AND SUBTYPE$ IN ('UTAB', 'STAB', 'VIEW', 'SYNOM')
AND ((SUBTYPE$ ='UTAB' AND CAST((INFO3 & 0x00FF & 0x003F) AS INT) not in (9, 27, 29, 25, 12, 7, 21, 23, 18, 5))
OR SUBTYPE$ in ('STAB', 'VIEW', 'SYNOM'))) TABS
WHERE TABS.SCHID = SCHEMAS.ID AND SF_CHECK_PRIV_OPT(UID(), CURRENT_USERTYPE(), TABS.ID, SCHEMAS.PID, -1, TABS.ID) = 1;`

	var count int64
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, table, err := m.catalogTarget(stmt)
		if err != nil {
			return err
		}
		return m.DB.Raw(tableSQL, schemaName, table).Row().Scan(&count)
	})
	return err == nil && count > 0, err
}

// RenameTable renames a table.
func (m Migrator) RenameTable(oldName, newName interface{}) error {
	return m.Migrator.RenameTable(oldName, newName)
}

// GetTables lists the tables of the current schema.
func (m Migrator) GetTables() (tableList []string, err error) {
	const tableSQL = `SELECT /*+ MAX_OPT_N_TABLES(5) */ TABS.NAME FROM
(SELECT ID, PID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = ?) SCHEMAS,
(SELECT ID, SCHID, NAME FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCHOBJ' AND SUBTYPE$ IN ('UTAB', 'STAB', 'VIEW', 'SYNOM')
AND ((SUBTYPE$ ='UTAB' AND CAST((INFO3 & 0x00FF & 0x003F) AS INT) not in (9, 27, 29, 25, 12, 7, 21, 23, 18, 5))
OR SUBTYPE$ in ('STAB', 'VIEW', 'SYNOM'))) TABS
WHERE TABS.SCHID = SCHEMAS.ID AND SF_CHECK_PRIV_OPT(UID(), CURRENT_USERTYPE(), TABS.ID, SCHEMAS.PID, -1, TABS.ID) = 1;`

	schemaName, err := m.currentDatabase()
	if err != nil {
		return nil, err
	}
	err = m.DB.Raw(tableSQL, schemaName).Scan(&tableList).Error
	return
}

// AddColumn adds a column and attaches its comment.
func (m Migrator) AddColumn(dst interface{}, field string) error {
	if err := m.Migrator.AddColumn(dst, field); err != nil {
		return err
	}

	return m.RunWithValue(dst, func(stmt *gorm.Statement) error {
		if stmt.Schema == nil {
			return nil
		}
		if field := stmt.Schema.LookUpField(field); field != nil {
			if err := m.alterColumnComment(stmt, field); err != nil {
				return err
			}
		}
		return nil
	})
}

// DropColumn drops a column.
func (m Migrator) DropColumn(dst interface{}, field string) error {
	return m.Migrator.DropColumn(dst, field)
}

// AlterColumn alters a column to match its model field.
func (m Migrator) AlterColumn(value interface{}, field string) error {
	return m.alterColumn(value, field, false)
}

// alterColumn issues ALTER TABLE ... MODIFY. When the column is already
// covered by a UNIQUE constraint the UNIQUE keyword is omitted, because
// DaMeng rejects adding a duplicate UNIQUE constraint.
func (m Migrator) alterColumn(value interface{}, field string, containsUnique bool) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema == nil {
			return fmt.Errorf("alterColumn requires a model value, got %T", value)
		}
		f := stmt.Schema.LookUpField(field)
		if f == nil {
			return fmt.Errorf("failed to look up field with name: %s", field)
		}
		typeof := m.FullDataTypeOf(f)
		if !f.NotNull && !f.PrimaryKey {
			typeof.SQL += " NULL"
		}
		if containsUnique && f.Unique {
			typeof.SQL = strings.Replace(typeof.SQL, " UNIQUE", "", 1)
		}
		if err := m.DB.Exec(
			"ALTER TABLE ? MODIFY ? ?",
			m.CurrentTable(stmt),
			clause.Column{Name: f.DBName},
			typeof,
		).Error; err != nil {
			return err
		}
		return m.alterColumnComment(stmt, f)
	})
}

// alterColumnComment issues COMMENT ON COLUMN for fields with a comment tag.
func (m Migrator) alterColumnComment(stmt *gorm.Statement, field *schema.Field) error {
	if field.Comment == "" {
		return nil
	}
	return m.DB.Exec(
		"COMMENT ON COLUMN ?.? IS ?",
		m.CurrentTable(stmt),
		clause.Column{Name: field.DBName},
		gorm.Expr(m.Migrator.Dialector.Explain("?", field.Comment)),
	).Error
}

// MigrateColumn migrates a single column when its definition drifted from
// the model. It mirrors gorm's default logic but routes changes through
// alterColumn so existing UNIQUE constraints are preserved.
func (m Migrator) MigrateColumn(dst interface{}, field *schema.Field, columnType gorm.ColumnType) error {
	fullDataType := strings.TrimSpace(strings.ToLower(m.DB.Migrator().FullDataTypeOf(field).SQL))
	realDataType := strings.ToLower(columnType.DatabaseTypeName())
	containsUnique := false
	if unique, ok := columnType.Unique(); ok {
		containsUnique = unique
	}

	alterColumn := m.columnTypeDrifted(field, columnType, fullDataType, realDataType)
	alterColumn = alterColumn || m.columnNullableDrifted(field, columnType)
	alterColumn = alterColumn || m.columnUniqueDrifted(field, columnType)
	alterColumn = alterColumn || m.columnDefaultDrifted(field, columnType)
	alterColumn = alterColumn || m.columnCommentDrifted(field, columnType)

	if alterColumn && !field.IgnoreMigration {
		return m.alterColumn(dst, field.DBName, containsUnique)
	}

	return nil
}

// columnTypeDrifted compares the modeled type (and its aliases), size and
// precision against the column reported by the database.
func (m Migrator) columnTypeDrifted(field *schema.Field, columnType gorm.ColumnType, fullDataType, realDataType string) bool {
	isSameType := fullDataType == realDataType

	if !field.PrimaryKey && !strings.HasPrefix(fullDataType, realDataType) {
		// check type aliases
		for _, alias := range m.DB.Migrator().GetTypeAliases(realDataType) {
			if strings.HasPrefix(fullDataType, alias) {
				isSameType = true
				break
			}
		}
		if !isSameType {
			return true
		}
	}

	if isSameType {
		return false
	}

	// check size
	if length, ok := columnType.Length(); length != int64(field.Size) {
		if length > 0 && field.Size > 0 {
			return true
		}
		// has size in data type and not equal
		matches := regFullDataType.FindAllStringSubmatch(fullDataType, -1)
		if !field.PrimaryKey && len(matches) == 1 && matches[0][1] != fmt.Sprint(length) && ok {
			return true
		}
	}

	// check precision
	if precision, _, ok := columnType.DecimalSize(); ok && int64(field.Precision) != precision {
		if regexp.MustCompile(fmt.Sprintf("[^0-9]%d[^0-9]", field.Precision)).MatchString(m.Migrator.DataTypeOf(field)) {
			return true
		}
	}

	return false
}

// columnNullableDrifted compares the NULL/NOT NULL state. Database nullable
// equals model NotNull means the flags are inverted.
func (m Migrator) columnNullableDrifted(field *schema.Field, columnType gorm.ColumnType) bool {
	nullable, ok := columnType.Nullable()
	if !ok || nullable != field.NotNull {
		return false
	}
	return !field.PrimaryKey
}

// columnUniqueDrifted compares the UNIQUE state.
func (m Migrator) columnUniqueDrifted(field *schema.Field, columnType gorm.ColumnType) bool {
	unique, ok := columnType.Unique()
	if !ok || unique == field.Unique {
		return false
	}
	return !field.PrimaryKey
}

// columnDefaultDrifted compares the default value, treating time functions
// case-insensitively.
func (m Migrator) columnDefaultDrifted(field *schema.Field, columnType gorm.ColumnType) bool {
	if field.PrimaryKey {
		return false
	}
	currentDefaultNotNull := field.HasDefaultValue && (field.DefaultValueInterface != nil || !strings.EqualFold(field.DefaultValue, "NULL"))
	dv, dvNotNull := columnType.DefaultValue()
	if dvNotNull && !currentDefaultNotNull {
		// default value -> null
		return true
	}
	if !dvNotNull && currentDefaultNotNull {
		// null -> default value
		return true
	}
	if (field.GORMDataType != schema.Time && dv != field.DefaultValue) ||
		(field.GORMDataType == schema.Time && !strings.EqualFold(strings.TrimSuffix(dv, "()"), strings.TrimSuffix(field.DefaultValue, "()"))) {
		// default value not equal
		// not both null
		return currentDefaultNotNull || dvNotNull
	}
	return false
}

// columnCommentDrifted compares the column comment.
func (m Migrator) columnCommentDrifted(field *schema.Field, columnType gorm.ColumnType) bool {
	comment, ok := columnType.Comment()
	if !ok || comment == field.Comment {
		return false
	}
	return !field.PrimaryKey
}

// HasColumn reports whether the column exists. The field name may be either
// a DB column name or a Go struct field name.
func (m Migrator) HasColumn(value interface{}, field string) bool {
	exists, err := m.hasColumn(value, field)
	_ = m.DB.AddError(err)
	return exists
}

func (m Migrator) hasColumn(value interface{}, field string) (bool, error) {
	const columnSQL = `SELECT /*+ MAX_OPT_N_TABLES(5) */ COUNT(DISTINCT COLS.NAME) FROM
(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = ?) SCHS,
(SELECT ID, SCHID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCHOBJ' AND SUBTYPE$ IN ('UTAB', 'STAB', 'VIEW') AND NAME = ?) TABS,
(SELECT NAME, ID FROM SYS.SYSCOLUMNS WHERE NAME = ?) COLS
WHERE TABS.ID = COLS.ID AND SCHS.ID = TABS.SCHID;`

	var count int64
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, table, err := m.catalogTarget(stmt)
		if err != nil {
			return err
		}
		columnName := field
		if stmt.Schema != nil {
			if f := stmt.Schema.LookUpField(field); f != nil {
				columnName = f.DBName
			}
		}
		return m.DB.Raw(columnSQL, schemaName, table, columnName).Row().Scan(&count)
	})
	return err == nil && count > 0, err
}

// RenameColumn renames a column.
func (m Migrator) RenameColumn(value interface{}, oldName, newName string) error {
	return m.Migrator.RenameColumn(value, oldName, newName)
}

// ColumnTypes returns the column metadata of a table, combining the raw
// database/sql types with primary key / unique information from the system
// catalog.
func (m Migrator) ColumnTypes(dst interface{}) ([]gorm.ColumnType, error) {
	columnTypes := make([]gorm.ColumnType, 0)
	execErr := m.RunWithValue(dst, func(stmt *gorm.Statement) error {
		currentDatabase, table, err := m.catalogTarget(stmt)
		if err != nil {
			return err
		}
		var (
			columnTypeSQL = `SELECT /*+ MAX_OPT_N_TABLES(5) */ COLS.NAME, COLS.DEFVAL FROM
(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = ?) SCHS,
(SELECT ID, SCHID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCHOBJ' AND SUBTYPE$ IN ('UTAB', 'STAB', 'VIEW') AND NAME = ?) TABS,
SYS.SYSCOLUMNS COLS
WHERE TABS.ID=COLS.ID AND SCHS.ID = TABS.SCHID`
			columnConsSQL = `SELECT /*+ MAX_OPT_N_TABLES(5) */ COLS.NAME, LNNVL(CONS.TYPE$!='P'), LNNVL(CONS.TYPE$!='U') FROM
(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = ?) SCHS,
(SELECT ID, SCHID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCHOBJ' AND SUBTYPE$ IN ('UTAB', 'STAB', 'VIEW') AND NAME = ?) TABS,
SYS.SYSCOLUMNS COLS,
SYS.SYSCONS CONS,
SYS.SYSINDEXES INDS
WHERE SCHS.ID=TABS.SCHID AND TABS.ID=COLS.ID AND COLS.ID=CONS.TABLEID and CONS.INDEXID=INDS.ID and SF_COL_IS_IDX_KEY(INDS.KEYNUM, INDS.KEYINFO, COLS.COLID)=1`
		)

		rows, err := m.DB.Session(&gorm.Session{}).Raw("SELECT * FROM ? WHERE 1 = 0", m.CurrentTable(stmt)).Rows()
		if err != nil {
			return err
		}

		rawColumnTypes, rawErr := rows.ColumnTypes()

		if err := rows.Close(); err != nil {
			return err
		}
		if rawErr != nil {
			return rawErr
		}

		consMap, err := m.queryColumnConstraints(columnConsSQL, currentDatabase, table)
		if err != nil {
			return err
		}

		return m.queryColumnTypes(columnTypeSQL, currentDatabase, table, consMap, rawColumnTypes, &columnTypes)
	})

	return columnTypes, execErr
}

// queryColumnConstraints reads the primary key / unique flags per column
// from the system catalog.
func (m Migrator) queryColumnConstraints(columnConsSQL, currentDatabase, table string) (map[string][][]bool, error) {
	cons, err := m.DB.Table(table).Raw(columnConsSQL, currentDatabase, table).Rows()
	if err != nil {
		return nil, err
	}
	defer func() { _ = cons.Close() }()

	consMap := make(map[string][][]bool)
	for cons.Next() {
		var (
			colName string
			values  = make([]bool, 2)
		)
		if scanErr := cons.Scan(&colName, &values[0], &values[1]); scanErr != nil {
			return nil, scanErr
		}
		consMap[colName] = append(consMap[colName], values)
	}
	return consMap, cons.Err()
}

// queryColumnTypes reads the catalog column rows and enriches them with the
// constraint flags and the raw database/sql column types.
func (m Migrator) queryColumnTypes(columnTypeSQL, currentDatabase, table string,
	consMap map[string][][]bool, rawColumnTypes []*sql.ColumnType, columnTypes *[]gorm.ColumnType) error {
	columns, err := m.DB.Table(table).Raw(columnTypeSQL, currentDatabase, table).Rows()
	if err != nil {
		return err
	}
	defer func() { _ = columns.Close() }()

	for columns.Next() {
		var column migrator.ColumnType
		if scanErr := columns.Scan(&column.NameValue, &column.DefaultValueValue); scanErr != nil {
			return scanErr
		}

		column.DefaultValueValue.String = strings.Trim(column.DefaultValueValue.String, "'")

		for key, value := range consMap {
			if key != column.NameValue.String {
				continue
			}
			for _, con := range value {
				if con[0] {
					column.PrimaryKeyValue.Bool = true
					column.PrimaryKeyValue.Valid = true
				}
				if con[1] {
					column.UniqueValue.Bool = true
					column.UniqueValue.Valid = true
				}
			}
			break
		}

		for _, c := range rawColumnTypes {
			if c.Name() == column.NameValue.String {
				column.SQLColumnType = c
				break
			}
		}
		if column.SQLColumnType == nil {
			return fmt.Errorf("dm: catalog column %q is missing from driver metadata for %s.%s", column.NameValue.String, currentDatabase, table)
		}

		*columnTypes = append(*columnTypes, column)
	}
	if err := columns.Err(); err != nil {
		return err
	}
	if len(*columnTypes) != len(rawColumnTypes) {
		return fmt.Errorf("dm: incomplete catalog metadata for %s.%s", currentDatabase, table)
	}
	return nil
}

// CreateView creates a view.
func (m Migrator) CreateView(name string, option gorm.ViewOption) error {
	return m.Migrator.CreateView(name, option)
}

// DropView drops a view.
func (m Migrator) DropView(name string) error {
	return m.Migrator.DropView(name)
}

// CreateConstraint creates a constraint.
func (m Migrator) CreateConstraint(dst interface{}, name string) error {
	return m.Migrator.CreateConstraint(dst, name)
}

// DropConstraint drops a constraint.
func (m Migrator) DropConstraint(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraint, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		if constraint != nil {
			name = constraint.GetName()
		}
		if stmt.TableExpr == nil {
			stmt.Table = table
		}
		return m.DB.Exec("ALTER TABLE ? DROP CONSTRAINT ?", m.CurrentTable(stmt), clause.Column{Name: name}).Error
	})
}

// HasConstraint reports whether the constraint exists.
func (m Migrator) HasConstraint(value interface{}, name string) bool {
	exists, err := m.hasConstraint(value, name)
	_ = m.DB.AddError(err)
	return exists
}

func (m Migrator) hasConstraint(value interface{}, name string) (bool, error) {
	const conSQL = `select count(CON_OBJ.NAME) from
(select ID from SYS.SYSOBJECTS where TYPE$='SCH' and NAME = ?) SCH_OBJ,
(select ID, SCHID, NAME from SYS.SYSOBJECTS where TYPE$='SCHOBJ' and SUBTYPE$ like '_TAB') TAB_OBJ,
(select ID, NAME from SYS.SYSOBJECTS where SUBTYPE$ = 'CONS' and NAME=?) CON_OBJ,
SYS.SYSCONS CONS
where CON_OBJ.ID=CONS.ID and TAB_OBJ.ID=CONS.TABLEID and TAB_OBJ.SCHID=SCH_OBJ.ID AND TAB_OBJ.NAME = ?;`

	var count int64
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		constraint, table := m.GuessConstraintInterfaceAndTable(stmt, name)
		if constraint != nil {
			name = constraint.GetName()
		}
		if stmt.TableExpr == nil {
			stmt.Table = table
		}
		schemaName, table, err := m.catalogTarget(stmt)
		if err != nil {
			return err
		}
		return m.DB.Raw(conSQL, schemaName, name, table).Row().Scan(&count)
	})
	return err == nil && count > 0, err
}

// CreateIndex creates an index.
func (m Migrator) CreateIndex(dst interface{}, name string) error {
	return m.Migrator.CreateIndex(dst, name)
}

// DropIndex drops an index.
func (m Migrator) DropIndex(value interface{}, name string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		if stmt.Schema != nil {
			if idx := stmt.Schema.LookIndex(name); idx != nil {
				name = idx.Name
			}
		}

		schemaName, _, err := m.catalogTarget(stmt)
		if err != nil {
			return err
		}
		return m.DB.Exec("DROP INDEX ?", clause.Table{Name: schemaName + "." + name}).Error
	})
}

// HasIndex reports whether the index exists.
func (m Migrator) HasIndex(value interface{}, name string) bool {
	exists, err := m.hasIndex(value, name)
	_ = m.DB.AddError(err)
	return exists
}

func (m Migrator) hasIndex(value interface{}, name string) (bool, error) {
	const indexSQL = `WITH USERS(ID) AS (SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = ?),
TAB(ID,SCHID) AS (SELECT ID, SCHID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCHOBJ' AND SUBTYPE$ = 'UTAB' AND NAME = ?)
SELECT COUNT(DISTINCT INDEX_NAME) FROM (
SELECT /*+ MAX_OPT_N_TABLES(5) */ OBJ_INDS.NAME AS INDEX_NAME FROM USERS, TAB, SYS.SYSINDEXES AS INDS, SYS.SYSCOLUMNS AS COLS,
(SELECT ID, PID, NAME FROM SYS.SYSOBJECTS WHERE SUBTYPE$='INDEX' AND NAME = ?) OBJ_INDS
WHERE TAB.ID =COLS.ID AND TAB.ID =OBJ_INDS.PID AND INDS.ID=OBJ_INDS.ID AND TAB.SCHID= USERS.ID
AND SF_COL_IS_IDX_KEY(INDS.KEYNUM, INDS.KEYINFO, COLS.COLID)=1
UNION SELECT OBJ_INDS.NAME AS INDEX_NAME FROM USERS, TAB, SYSCONTEXTINDEXES AS OBJ_INDS, SYS.SYSCOLUMNS AS COLS
WHERE TAB.ID = COLS.ID AND TAB.ID = OBJ_INDS.TABLEID AND COLS.COLID = OBJ_INDS.COLID AND TAB.SCHID = USERS.ID AND OBJ_INDS.NAME = ?)`

	var count int64
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, table, err := m.catalogTarget(stmt)
		if err != nil {
			return err
		}
		if stmt.Schema != nil {
			if idx := stmt.Schema.LookIndex(name); idx != nil {
				name = idx.Name
			}
		}
		return m.DB.Raw(indexSQL, schemaName, table, name, name).Row().Scan(&count)
	})
	return err == nil && count > 0, err
}

// RenameIndex renames an index.
func (m Migrator) RenameIndex(value interface{}, oldName, newName string) error {
	return m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, _, err := m.catalogTarget(stmt)
		if err != nil {
			return err
		}
		return m.DB.Exec(
			"ALTER INDEX ? RENAME TO ?",
			clause.Table{Name: schemaName + "." + oldName}, clause.Column{Name: newName},
		).Error
	})
}

// GetIndexes returns the indexes of a table, including full-text indexes.
func (m Migrator) GetIndexes(value interface{}) ([]gorm.Index, error) {
	const indexSQL = `WITH USERS(ID) AS (SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = ?),
TAB(ID,SCHID,NAME) AS (SELECT ID, SCHID, NAME FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCHOBJ' AND SUBTYPE$ = 'UTAB' AND NAME = ?)
SELECT /*+ MAX_OPT_N_TABLES(5) */ TAB.NAME AS TABLE_NAME, COLS.NAME AS COLUMN_NAME,
OBJ_INDS.NAME AS INDEX_NAME, CASE INDS.ISUNIQUE WHEN 'Y' THEN 0 ELSE 1 END AS NON_UNIQUE,
CASE OBJ_INDS.TYPE$ WHEN 'P' THEN 1 ELSE 0 END AS IS_PRIMARY FROM USERS, TAB, SYS.SYSINDEXES AS INDS, SYS.SYSCOLUMNS AS COLS,
(SELECT INDS.ID, INDS.PID, INDS.NAME, CONS.TYPE$ FROM SYS.SYSOBJECTS AS INDS LEFT JOIN SYS.SYSCONS AS CONS ON CONS.INDEXID=INDS.ID AND SUBTYPE$='INDEX') OBJ_INDS
WHERE TAB.ID =COLS.ID AND TAB.ID =OBJ_INDS.PID AND INDS.ID=OBJ_INDS.ID AND TAB.SCHID=USERS.ID AND SF_COL_IS_IDX_KEY(INDS.KEYNUM, INDS.KEYINFO, COLS.COLID)=1
UNION SELECT TAB.NAME AS TABLE_NAME, COLS.NAME AS COLUMN_NAME, OBJ_INDS.NAME AS INDEX_NAME, 1 AS NON_UNIQUE, 0 AS IS_PRIMARY FROM
USERS, TAB, SYSCONTEXTINDEXES AS OBJ_INDS, SYS.SYSCOLUMNS AS COLS WHERE
TAB.ID = COLS.ID AND TAB.ID = OBJ_INDS.TABLEID AND COLS.COLID = OBJ_INDS.COLID AND TAB.SCHID = USERS.ID;`

	indexes := make([]gorm.Index, 0)
	err := m.RunWithValue(value, func(stmt *gorm.Statement) error {
		schemaName, table, err := m.catalogTarget(stmt)
		if err != nil {
			return err
		}
		result := make([]*Index, 0)
		if scanErr := m.DB.Raw(indexSQL, schemaName, table).Scan(&result).Error; scanErr != nil {
			return scanErr
		}
		indexMap := groupByIndexName(result)
		for _, idx := range indexMap {
			tempIdx := &migrator.Index{
				TableName: idx[0].TableName,
				NameValue: idx[0].IndexName,
				PrimaryKeyValue: sql.NullBool{
					Bool:  idx[0].Primary,
					Valid: true,
				},
				UniqueValue: sql.NullBool{
					Bool:  !idx[0].NonUnique,
					Valid: true,
				},
			}
			for _, x := range idx {
				tempIdx.ColumnList = append(tempIdx.ColumnList, x.ColumnName)
			}
			indexes = append(indexes, tempIdx)
		}
		return nil
	})
	return indexes, err
}

// Index is a row of the GetIndexes catalog query.
type Index struct {
	TableName  string `gorm:"column:TABLE_NAME"`
	ColumnName string `gorm:"column:COLUMN_NAME"`
	IndexName  string `gorm:"column:INDEX_NAME"`
	NonUnique  bool   `gorm:"column:NON_UNIQUE"`
	Primary    bool   `gorm:"column:IS_PRIMARY"`
}

// groupByIndexName groups catalog rows by index name.
func groupByIndexName(indexList []*Index) map[string][]*Index {
	columnIndexMap := make(map[string][]*Index, len(indexList))
	for _, idx := range indexList {
		columnIndexMap[idx.IndexName] = append(columnIndexMap[idx.IndexName], idx)
	}
	return columnIndexMap
}
