package dm

import (
	"database/sql"
	"database/sql/driver"
	"reflect"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

// Create is the DaMeng create callback registered in Initialize. It replaces
// gorm:create to support identity columns, MERGE INTO upserts (GormModeDM)
// and MySQL/PostgreSQL-compatible upserts (GormModeMySQL/GormModePostgres).
func Create(db *gorm.DB) {
	if db.Error != nil {
		return
	}

	if db.Statement.Schema != nil && !db.Statement.Unscoped {
		for _, c := range db.Statement.Schema.CreateClauses {
			db.Statement.AddClause(c)
		}
	}

	var onConflict clause.OnConflict
	var hasConflict bool
	var rowCount int64
	if db.Statement.SQL.String() == "" {
		values := callbacks.ConvertToCreateValues(db.Statement)
		c := db.Statement.Clauses["ON CONFLICT"]
		onConflict, hasConflict = c.Expression.(clause.OnConflict)

		gormMode := dialectorGormMode(db)
		if gormMode == GormModeMySQL || gormMode == GormModePostgres {
			defer applyIdentityInsert(db)()
			Upsert(gormMode, db, onConflict, values)
			rowCount = int64(len(values.Values))
			hasConflict = false
		} else {
			// MERGE INTO requires explicit conflict columns; without them the
			// statement degrades to a plain INSERT.
			if hasConflict && onConflict.Columns == nil {
				hasConflict = false
			}

			if hasConflict {
				MergeCreate(db, onConflict, values)
			} else {
				defer applyIdentityInsert(db)()
				db.Statement.AddClauseIfNotExists(clause.Insert{})
				normalInsert(db, values)
			}
		}
	}

	if db.DryRun || db.Error != nil {
		return
	}

	if hasConflict {
		queryMergeResult(db)
		return
	}

	execInsert(db, rowCount)
}

// dialectorGormMode returns Config.GormMode, falling back to GormModeDM when
// the statement's dialector is not this package's Dialector.
func dialectorGormMode(db *gorm.DB) int {
	if d, ok := db.Statement.Dialector.(*Dialector); ok && d.Config != nil {
		return d.GormMode
	}
	return GormModeDM
}

// applyIdentityInsert enables IDENTITY_INSERT when the caller provides
// explicit values for the auto-increment primary key. The returned cleanup
// turns it back off; the caller must defer it so the OFF statement runs
// after the INSERT has executed, mirroring the official dialect.
func applyIdentityInsert(db *gorm.DB) func() {
	if db.Statement.Schema == nil {
		return noopCleanup
	}
	field := db.Statement.Schema.PrioritizedPrimaryField
	if field == nil || !field.AutoIncrement || !initSetIdentity(db, field) {
		return noopCleanup
	}
	if db.DryRun || db.Error != nil {
		return noopCleanup
	}
	identityOn, identityOnSQL := setIdentityOn(db)
	if !identityOn {
		return noopCleanup
	}
	return func() { setIdentityOff(db, identityOnSQL) }
}

func noopCleanup() {}

// initSetIdentity reports whether the primary field carries a non-zero value,
// i.e. the insert supplies the identity column explicitly.
func initSetIdentity(db *gorm.DB, field *schema.Field) bool {
	setIdentityInsert := false
	switch db.Statement.ReflectValue.Kind() {
	case reflect.Struct:
		_, isZero := field.ValueOf(db.Statement.Context, db.Statement.ReflectValue)
		setIdentityInsert = !isZero
	case reflect.Slice, reflect.Array:
		if db.Statement.ReflectValue.Len() > 0 {
			obj := db.Statement.ReflectValue.Index(0)
			if reflect.Indirect(obj).Kind() == reflect.Struct {
				_, isZero := field.ValueOf(db.Statement.Context, obj)
				setIdentityInsert = !isZero
			}
		}
	}
	return setIdentityInsert
}

// setIdentityOn executes SET IDENTITY_INSERT <table> ON. It returns false
// when the table has no identity column (-2717); in that case the
// AutoIncrement flag is cleared so later inserts skip this round trip.
func setIdentityOn(db *gorm.DB) (bool, string) {
	db.Statement.SQL.Reset()
	_, _ = db.Statement.WriteString("SET IDENTITY_INSERT ")
	db.Statement.WriteQuoted(getTableName(db.Statement))
	_, _ = db.Statement.WriteString(" ON;")
	_, err := db.Statement.ConnPool.ExecContext(db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...)
	if db.AddError(err) != nil {
		if isDmErrorCode(err, errCodeNoIdentityColumn) && db.Statement.Schema != nil &&
			db.Statement.Schema.PrioritizedPrimaryField != nil {
			db.Statement.Schema.PrioritizedPrimaryField.AutoIncrement = false
			db.Error = nil
			return false, ""
		}
		return false, ""
	}
	return true, db.Statement.SQL.String()
}

// setIdentityOff executes SET IDENTITY_INSERT <table> OFF and restores the
// statement SQL to the full identityOn/main/identityOff chain for logging.
func setIdentityOff(db *gorm.DB, identityOnSQL string) {
	mainSQL := db.Statement.SQL.String()
	db.Statement.SQL.Reset()
	_, _ = db.Statement.WriteString("SET IDENTITY_INSERT ")
	db.Statement.WriteQuoted(getTableName(db.Statement))
	_, _ = db.Statement.WriteString(" OFF;")
	_, _ = db.Statement.ConnPool.ExecContext(db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...)
	identityOffSQL := db.Statement.SQL.String()
	db.Statement.SQL.Reset()
	_, _ = db.Statement.WriteString(identityOnSQL + "\n" + mainSQL + "\n" + identityOffSQL)
}

// normalInsert builds a plain INSERT INTO ... VALUES ... statement.
func normalInsert(db *gorm.DB, values clause.Values) {
	db.Statement.SQL.Reset()
	_, _ = db.Statement.WriteString("INSERT INTO ")
	db.Statement.WriteQuoted(getTableName(db.Statement))
	db.Statement.AddClause(values)
	if values, ok := db.Statement.Clauses["VALUES"].Expression.(clause.Values); ok {
		if len(values.Columns) > 0 {
			_, _ = db.Statement.WriteString(" (")
			for idx, column := range values.Columns {
				if idx > 0 {
					_ = db.Statement.WriteByte(',')
				}
				db.Statement.WriteQuoted(column)
			}
			_ = db.Statement.WriteByte(')')

			_, _ = db.Statement.WriteString(" VALUES ")

			for idx, value := range values.Values {
				if idx > 0 {
					_ = db.Statement.WriteByte(',')
				}

				_ = db.Statement.WriteByte('(')
				db.Statement.AddVar(db.Statement, value...)
				_ = db.Statement.WriteByte(')')
			}

			_, _ = db.Statement.WriteString(";")
		} else {
			_, _ = db.Statement.WriteString("DEFAULT VALUES")
		}
	}
}

// MergeCreate builds a DaMeng MERGE INTO statement for GormModeDM upserts.
func MergeCreate(db *gorm.DB, onConflict clause.OnConflict, values clause.Values) {
	_, _ = db.Statement.WriteString("MERGE INTO ")
	db.Statement.WriteQuoted(getTableName(db.Statement))
	_, _ = db.Statement.WriteString(" USING (")
	for idx, value := range values.Values {
		if idx > 0 {
			_, _ = db.Statement.WriteString(" UNION ALL ")
		}

		_, _ = db.Statement.WriteString("SELECT ")
		db.Statement.AddVar(db.Statement, value...)
		_, _ = db.Statement.WriteString(" FROM DUAL")
	}

	_, _ = db.Statement.WriteString(`) AS "excluded" (`)
	for idx, column := range values.Columns {
		if idx > 0 {
			_ = db.Statement.WriteByte(',')
		}
		db.Statement.WriteQuoted(column.Name)
	}
	_, _ = db.Statement.WriteString(") ON ")

	var where clause.Where
	for _, col := range onConflict.Columns {
		where.Exprs = append(where.Exprs, clause.Eq{
			Column: clause.Column{Table: getTableName(db.Statement), Name: col.Name},
			Value:  clause.Column{Table: "excluded", Name: col.Name},
		})
	}
	where.Build(db.Statement)

	writeMergeUpdate(db, onConflict)
	writeMergeInsert(db, values)
	writeMergeIdentitySelect(db)
}

// writeMergeUpdate emits WHEN MATCHED THEN UPDATE SET, dropping assignments
// on columns used in the ON condition (DaMeng error -4064).
func writeMergeUpdate(db *gorm.DB, onConflict clause.OnConflict) {
	if len(onConflict.DoUpdates) == 0 {
		return
	}
	withoutOnColumns := make([]clause.Assignment, 0, len(onConflict.DoUpdates))
outer:
	for _, assignment := range onConflict.DoUpdates {
		if db.Statement.Schema == nil {
			for _, col := range onConflict.Columns {
				if assignment.Column.Name == col.Name {
					continue outer
				}
			}
		} else {
			for _, field := range db.Statement.Schema.PrimaryFields {
				if assignment.Column.Name == field.DBName {
					continue outer
				}
			}
		}
		withoutOnColumns = append(withoutOnColumns, assignment)
	}
	onConflict.DoUpdates = clause.Set(withoutOnColumns)
	if len(onConflict.DoUpdates) > 0 {
		_, _ = db.Statement.WriteString(" WHEN MATCHED THEN UPDATE SET ")
		onConflict.DoUpdates.Build(db.Statement)
	}
}

// writeMergeInsert emits WHEN NOT MATCHED THEN INSERT, skipping the
// auto-increment primary key column.
func writeMergeInsert(db *gorm.DB, values clause.Values) {
	_, _ = db.Statement.WriteString(" WHEN NOT MATCHED THEN INSERT (")

	isIdentityColumn := func(columnName string) bool {
		return db.Statement.Schema != nil &&
			db.Statement.Schema.PrioritizedPrimaryField != nil &&
			db.Statement.Schema.PrioritizedPrimaryField.AutoIncrement &&
			db.Statement.Schema.PrioritizedPrimaryField.DBName == columnName
	}

	written := false
	for _, column := range values.Columns {
		if isIdentityColumn(column.Name) {
			continue
		}
		if written {
			_ = db.Statement.WriteByte(',')
		}
		written = true
		db.Statement.WriteQuoted(column.Name)
	}

	_, _ = db.Statement.WriteString(") VALUES (")

	written = false
	for _, column := range values.Columns {
		if isIdentityColumn(column.Name) {
			continue
		}
		if written {
			_ = db.Statement.WriteByte(',')
		}
		written = true
		db.Statement.WriteQuoted(clause.Column{
			Table: "excluded",
			Name:  column.Name,
		})
	}

	_, _ = db.Statement.WriteString(")")
	_, _ = db.Statement.WriteString(";")
}

// writeMergeIdentitySelect appends the SELECT used to fetch the identity
// value, because MERGE INTO statements have no LastInsertId.
func writeMergeIdentitySelect(db *gorm.DB) {
	if db.Statement.Schema == nil || db.Statement.Schema.PrioritizedPrimaryField == nil ||
		!db.Statement.Schema.PrioritizedPrimaryField.AutoIncrement {
		return
	}
	_, _ = db.Statement.WriteString("SELECT ")
	db.Statement.WriteQuoted(db.Statement.Schema.PrioritizedPrimaryField.DBName)
	_, _ = db.Statement.WriteString(" FROM ")
	db.Statement.WriteQuoted(getTableName(db.Statement))
	_, _ = db.Statement.WriteString(" ORDER BY ")
	db.Statement.WriteQuoted(db.Statement.Schema.PrioritizedPrimaryField.DBName)
	_, _ = db.Statement.WriteString(" DESC LIMIT 1;")
}

// Upsert builds a PL/SQL block emulating MySQL's ON DUPLICATE KEY UPDATE
// (GormModeMySQL) or PostgreSQL's ON CONFLICT DO UPDATE (GormModePostgres):
// every row is inserted with EXECUTE IMMEDIATE and a DUP_VAL_ON_INDEX
// exception falls back to a dynamically built UPDATE on the indexed columns.
func Upsert(gormMode int, db *gorm.DB, onConflict clause.OnConflict, values clause.Values) {
	if !onConflict.UpdateAll && len(onConflict.DoUpdates) == 0 {
		// Nothing to update on conflict: a plain INSERT is enough.
		normalInsert(db, values)
		return
	}

	schemaName, tableName := splitTableName(getTableName(db.Statement))
	rowCount := len(values.Values)

	db.Statement.SQL.Reset()
	writeUpsertDeclare(db, onConflict, values, gormMode, schemaName, tableName, rowCount)
	writeUpsertUpdateDecl(db, onConflict, values, schemaName, tableName, rowCount)
	_, _ = db.Statement.WriteString(` BEGIN `)
	for i := 0; i < rowCount; i++ {
		writeUpsertRowBlock(db, onConflict, values, gormMode, schemaName, tableName, i)
	}
	writeUpsertIdentSelect(db, schemaName, tableName)
}

// writeUpsertDeclare writes the DECLARE section: one typed variable per
// column and row, the update-assignment variables and the bookkeeping
// variables for the fallback UPDATE.
func writeUpsertDeclare(db *gorm.DB, onConflict clause.OnConflict, values clause.Values,
	gormMode int, schemaName, tableName string, rowCount int) {
	_, _ = db.Statement.WriteString("/*DMGORM-UPSERT*/DECLARE ")
	for i := 0; i < rowCount; i++ {
		for _, columnName := range values.Columns {
			_, _ = db.Statement.WriteString(appendVariableName(columnName.Name, strconv.Itoa(i)) + " ")
			appendCustomSchema(schemaName, db)
			db.Statement.WriteQuoted(tableName)
			_ = db.Statement.WriteByte('.')
			db.Statement.WriteQuoted(columnName.Name)
			_, _ = db.Statement.WriteString("%TYPE := ?")
			_ = db.Statement.WriteByte(';')
		}
		BindVar(db.Statement, values.Values[i])

		// Explicit update values (clause.Assignment with a literal value)
		// need their own bind variables.
		for _, conflictName := range onConflict.DoUpdates {
			_, _ = db.Statement.WriteString(appendVariableName(conflictName.Column.Name, strconv.Itoa(i)) + "_UPDATE ")
			appendCustomSchema(schemaName, db)
			db.Statement.WriteQuoted(tableName)
			_ = db.Statement.WriteByte('.')
			db.Statement.WriteQuoted(conflictName.Column.Name)
			if _, ok := conflictName.Value.(clause.Column); !ok {
				_, _ = db.Statement.WriteString("%TYPE := ?")
				BindVar(db.Statement, conflictName.Value)
			} else {
				_, _ = db.Statement.WriteString("%TYPE := ")
				_, _ = db.Statement.WriteString(appendVariableName(conflictName.Column.Name, strconv.Itoa(i)))
			}
			_ = db.Statement.WriteByte(';')
		}
	}
	// v_upsert_flag stays 1 when every row took the INSERT path; only then
	// may the identity value be selected back.
	_, _ = db.Statement.WriteString(" v_upsert_flag TINYINT := 1; ")
	_, _ = db.Statement.WriteString(" TYPE emp_tab IS TABLE OF VARCHAR2; ")
	if gormMode == GormModePostgres && len(onConflict.Columns) > 0 {
		_, _ = db.Statement.WriteString(` v_columns emp_tab := emp_tab(`)
		for i, v := range onConflict.Columns {
			_, _ = db.Statement.WriteString(" '" + v.Name + "' ")
			if i < len(onConflict.Columns)-1 {
				_, _ = db.Statement.WriteString(", ")
			}
		}
		_, _ = db.Statement.WriteString(`); v_diff1 emp_tab; v_diff2 emp_tab; `)
	}
}

// writeUpsertUpdateDecl declares, per row, the dynamic UPDATE statement
// template and the variables holding its WHERE condition and conflict index
// name.
func writeUpsertUpdateDecl(db *gorm.DB, onConflict clause.OnConflict, values clause.Values,
	schemaName, tableName string, rowCount int) {
	for i := 0; i < rowCount; i++ {
		rowIndex := strconv.Itoa(i)
		_, _ = db.Statement.WriteString("v_emp_data" + rowIndex + " emp_tab; ")
		_, _ = db.Statement.WriteString("v_sql_update" + rowIndex)
		_, _ = db.Statement.WriteString(" VARCHAR2(32767) := 'DECLARE ")

		for _, columnName := range values.Columns {
			// Inner variables are suffixed with X to avoid clashing with the
			// outer declare-section variables.
			_, _ = db.Statement.WriteString(strings.ToUpper(columnName.Name) + "X ")
			appendCustomSchema(schemaName, db)
			db.Statement.WriteQuoted(tableName)
			_ = db.Statement.WriteByte('.')
			db.Statement.WriteQuoted(columnName.Name)
			_, _ = db.Statement.WriteString("%TYPE := :")
			_, _ = db.Statement.WriteString(appendVariableName(columnName.Name, rowIndex))
			_, _ = db.Statement.WriteString("; ")
		}
		_, _ = db.Statement.WriteString("BEGIN UPDATE ")
		appendCustomSchema(schemaName, db)
		db.Statement.WriteQuoted(tableName)
		_, _ = db.Statement.WriteString(" SET ")

		if onConflict.UpdateAll {
			for idx, columnName := range values.Columns {
				if idx > 0 {
					_, _ = db.Statement.WriteString(", ")
				}
				db.Statement.WriteQuoted(columnName.Name)
				_, _ = db.Statement.WriteString(" = :")
				_, _ = db.Statement.WriteString(appendVariableName(columnName.Name, rowIndex))
			}
		} else if len(onConflict.DoUpdates) > 0 {
			for idx, conflictName := range onConflict.DoUpdates {
				if idx > 0 {
					_, _ = db.Statement.WriteString(", ")
				}
				db.Statement.WriteQuoted(conflictName.Column.Name)
				_, _ = db.Statement.WriteString(" = :")
				_, _ = db.Statement.WriteString(appendVariableName(conflictName.Column.Name, rowIndex) + "_UPDATE")
			}
		}
		_, _ = db.Statement.WriteString(` WHERE ';`)
		_, _ = db.Statement.WriteString(` v_condition` + rowIndex + ` VARCHAR2(32767) := '';`)
		_, _ = db.Statement.WriteString(` v_index_name` + rowIndex + ` VARCHAR2(32767);`)
	}
}

// writeUpsertRowBlock writes the per-row BEGIN ... EXCEPTION ... END block:
// the EXECUTE IMMEDIATE INSERT and, on DUP_VAL_ON_INDEX, the index lookup,
// the WHERE condition assembly and the fallback UPDATE execution.
func writeUpsertRowBlock(db *gorm.DB, onConflict clause.OnConflict, values clause.Values,
	gormMode int, schemaName, tableName string, rowIdx int) {
	rowIndex := strconv.Itoa(rowIdx)

	_, _ = db.Statement.WriteString(` BEGIN `)
	_, _ = db.Statement.WriteString(` EXECUTE IMMEDIATE 'INSERT INTO `)
	appendCustomSchema(schemaName, db)
	db.Statement.WriteQuoted(tableName)
	_, _ = db.Statement.WriteString(" (")
	for idx, columnName := range values.Columns {
		if idx > 0 {
			_, _ = db.Statement.WriteString(", ")
		}
		db.Statement.WriteQuoted(columnName.Name)
	}
	_, _ = db.Statement.WriteString(") VALUES(")
	for idx, columnName := range values.Columns {
		if idx > 0 {
			_, _ = db.Statement.WriteString(", ")
		}
		_ = db.Statement.WriteByte(':')
		_, _ = db.Statement.WriteString(appendVariableName(columnName.Name, rowIndex))
	}
	_, _ = db.Statement.WriteString(");' USING ")
	for idx, columnName := range values.Columns {
		if idx > 0 {
			_, _ = db.Statement.WriteString(", ")
		}
		_, _ = db.Statement.WriteString(appendVariableName(columnName.Name, rowIndex))
	}
	_, _ = db.Statement.WriteString(`; `)

	_, _ = db.Statement.WriteString(`EXCEPTION WHEN DUP_VAL_ON_INDEX THEN `)
	_, _ = db.Statement.WriteString("v_upsert_flag := 0;")
	writeUpsertIndexLookup(db, onConflict, gormMode, schemaName, tableName, rowIndex)
	writeUpsertConditionLoop(db, onConflict, gormMode, rowIndex)
	writeUpsertFallbackUpdate(db, onConflict, values, rowIndex)
}

// writeUpsertIndexLookup extracts the violated index name from SQLERRM and
// bulk-collects its columns from the DaMeng catalog (and user_indexes for
// unique indexes).
func writeUpsertIndexLookup(db *gorm.DB, onConflict clause.OnConflict,
	gormMode int, schemaName, tableName, rowIndex string) {
	_, _ = db.Statement.WriteString(`v_index_name` + rowIndex +
		` := (SELECT SUBSTR(SQLERRM, INSTR(SQLERRM, '[', -1, 1) + 1, INSTR(SQLERRM, ']', -1, 1) - INSTR(SQLERRM, '[', -1, 1) - 1));
 SELECT COLS.NAME
 BULK COLLECT INTO v_emp_data` + rowIndex)
	_, _ = db.Statement.WriteString(` from
 SYSCOLUMNS COLS,
 SYSCONS CONS,
 SYSINDEXES INDS WHERE CONS.ID = (select ID from SYSOBJECTS WHERE NAME = v_index_name` + rowIndex)
	if gormMode == GormModePostgres && len(onConflict.OnConstraint) > 0 {
		_, _ = db.Statement.WriteString(` AND NAME = '` + onConflict.OnConstraint + `' `)
	}

	// Without a schema name fall back to the current schema CURRENT_SCHID().
	if schemaName == "" {
		_, _ = db.Statement.WriteString(" AND SCHID = CURRENT_SCHID()) ")
		_, _ = db.Statement.WriteString(`AND COLS.ID = (select ID from SYSOBJECTS WHERE SCHID = CURRENT_SCHID() AND name = `)
	} else {
		_, _ = db.Statement.WriteString(" AND SCHID = (SELECT ID FROM SYSOBJECTS WHERE TYPE$='SCH' AND NAME = '")
		_, _ = db.Statement.WriteString(schemaName + "')) ")
		_, _ = db.Statement.WriteString(`AND COLS.ID = (select ID from SYSOBJECTS WHERE SCHID = (SELECT ID FROM SYSOBJECTS WHERE TYPE$='SCH' AND NAME = '`)
		_, _ = db.Statement.WriteString(schemaName + "') ")
		_, _ = db.Statement.WriteString(`AND name = `)
	}
	_, _ = db.Statement.WriteString("'" + tableName + "'")
	_, _ = db.Statement.WriteString(`)
 AND INDS.ID = CONS.INDEXID AND SF_COL_IS_IDX_KEY(INDS.KEYNUM, INDS.KEYINFO, COLS.COLID)= 1
 union
 SELECT c.column_name AS NAME  FROM user_indexes i JOIN USER_IND_COLUMNS c
 on i.index_name = c.index_name where i.index_name = v_index_name` + rowIndex + " ")
	if gormMode == GormModePostgres && len(onConflict.OnConstraint) > 0 {
		_, _ = db.Statement.WriteString(`AND i.index_name = '` + onConflict.OnConstraint + `' `)
	}
	_, _ = db.Statement.WriteString(`AND i.UNIQUENESS = 'UNIQUE' AND i.STATUS = 'VALID' AND i.TABLE_NAME = `)
	_, _ = db.Statement.WriteString("'" + tableName + "'")
	_, _ = db.Statement.WriteString(` AND I.TABLE_OWNER = `)
	if schemaName == "" {
		_, _ = db.Statement.WriteString(`(SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') FROM DUAL);`)
	} else {
		_, _ = db.Statement.WriteString("'" + schemaName + "';")
	}

	if gormMode == GormModePostgres && len(onConflict.Columns) > 0 {
		// The violated index columns must match ON CONFLICT's target columns.
		_, _ = db.Statement.WriteString(`BEGIN v_diff1 := v_emp_data` + rowIndex + ` multiset except v_columns; v_diff2 := v_columns multiset except v_emp_data` + rowIndex + `; `)
		_, _ = db.Statement.WriteString(`if v_diff1.COUNT > 0 OR v_diff2.COUNT > 0 THEN RAISE; END IF; END;`)
	}
}

// writeUpsertConditionLoop assembles the WHERE clause of the fallback UPDATE
// from the violated index columns, plus the optional ON CONFLICT WHERE
// conditions (GormModePostgres only).
func writeUpsertConditionLoop(db *gorm.DB, onConflict clause.OnConflict,
	gormMode int, rowIndex string) {
	_, _ = db.Statement.WriteString(`FOR i IN 1..v_emp_data` + rowIndex + `.COUNT LOOP
	v_condition` + rowIndex + `:= v_condition` + rowIndex + ` || ' "' || v_emp_data` + rowIndex + `(i) ||'" = :'|| UPPER(v_emp_data` + rowIndex + `(i)) || '_` + rowIndex + ` AND ';
 END LOOP;
 v_condition` + rowIndex + ` := v_condition` + rowIndex)
	if gormMode == GormModePostgres {
		appendWhereCondition(db, onConflict.Where)
		appendWhereCondition(db, onConflict.TargetWhere)
	}
	// v_emp_data0 is referenced unconditionally: row 0 always exists when any
	// row block is generated.
	_, _ = db.Statement.WriteString(` || '1 = ' || IF(v_emp_data0.COUNT = 0, 0, 1) || '; END;' ;
 v_sql_update` + rowIndex + ` := ` + `v_sql_update` + rowIndex + ` || ' ' || v_condition` + rowIndex + `;
 EXECUTE IMMEDIATE v_sql_update` + rowIndex + ` using `)
}

// writeUpsertFallbackUpdate emits the USING list of the fallback UPDATE's
// EXECUTE IMMEDIATE and closes the exception block.
func writeUpsertFallbackUpdate(db *gorm.DB, onConflict clause.OnConflict,
	values clause.Values, rowIndex string) {
	for idx, columnName := range values.Columns {
		if idx > 0 {
			_, _ = db.Statement.WriteString(", ")
		}
		_, _ = db.Statement.WriteString(appendVariableName(columnName.Name, rowIndex))
	}
	for _, conflictName := range onConflict.DoUpdates {
		_, _ = db.Statement.WriteString(", ")
		_, _ = db.Statement.WriteString(appendVariableName(conflictName.Column.Name, rowIndex) + "_UPDATE")
	}
	_, _ = db.Statement.WriteString(`;
WHEN OTHERS THEN
	RAISE;
END;`)
}

// writeUpsertIdentSelect selects the identity value back when every row took
// the INSERT path; -7019 (no identity column) is tolerated.
func writeUpsertIdentSelect(db *gorm.DB, schemaName, tableName string) {
	schemaTabName := tableName
	if schemaName != "" {
		schemaTabName = schemaName + "." + schemaTabName
	}
	_, _ = db.Statement.WriteString(`BEGIN
   IF v_upsert_flag = 1 THEN
	BEGIN
	SELECT IDENT_CURRENT('` + schemaTabName + `') from dual;
	EXCEPTION WHEN OTHERS THEN IF SQLCODE = -7019 THEN NULL; ELSE RAISE; END IF;
	END;
	END IF;
END;
`)
	_, _ = db.Statement.WriteString("END;")
}

// queryMergeResult executes a MERGE INTO statement, which returns the
// identity value as a result set instead of LastInsertId.
func queryMergeResult(db *gorm.DB) {
	rows, err := db.Statement.ConnPool.QueryContext(db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...)
	if db.AddError(err) != nil {
		return
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		var insertID int64
		_ = rows.Scan(&insertID)
		if insertID > 0 {
			setInsertID(db, insertID)
		}
	}
}

// execInsert executes a plain INSERT or PL/SQL upsert and backfills the
// auto-increment primary key when possible.
func execInsert(db *gorm.DB, rowCount int64) {
	result, err := db.Statement.ConnPool.ExecContext(db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...)
	if db.AddError(err) != nil {
		return
	}
	if dialectorGormMode(db) == GormModeMySQL {
		// The PL/SQL block reports per-row semantics, not raw rows affected.
		db.RowsAffected = rowCount
	} else {
		db.RowsAffected, _ = result.RowsAffected()
	}
	if db.RowsAffected == 0 || db.Statement.Schema == nil ||
		db.Statement.Schema.PrioritizedPrimaryField == nil ||
		!db.Statement.Schema.PrioritizedPrimaryField.HasDefaultValue {
		return
	}
	insertID, err := result.LastInsertId()
	if err != nil {
		_ = db.AddError(err)
		return
	}
	if insertID > 0 {
		setInsertID(db, insertID)
	}
}

// setInsertID backfills the auto-increment primary key on the inserted
// value(s), walking slices backwards because LastInsertId is the newest row.
func setInsertID(db *gorm.DB, insertID int64) {
	field := db.Statement.Schema.PrioritizedPrimaryField
	switch db.Statement.ReflectValue.Kind() {
	case reflect.Slice, reflect.Array:
		for i := db.Statement.ReflectValue.Len() - 1; i >= 0; i-- {
			rv := db.Statement.ReflectValue.Index(i)
			if reflect.Indirect(rv).Kind() != reflect.Struct {
				break
			}

			_, isZero := field.ValueOf(db.Statement.Context, rv)
			if isZero {
				_ = db.AddError(field.Set(db.Statement.Context, rv, insertID))
				insertID -= field.AutoIncrementIncrement
			}
		}
	case reflect.Struct:
		_, isZero := field.ValueOf(db.Statement.Context, db.Statement.ReflectValue)
		if isZero {
			_ = db.AddError(field.Set(db.Statement.Context, db.Statement.ReflectValue, insertID))
		}
	}
}

// appendWhereCondition appends an ON CONFLICT WHERE clause to the dynamic
// UPDATE condition.
func appendWhereCondition(db *gorm.DB, where clause.Where) {
	if len(where.Exprs) > 0 {
		_, _ = db.Statement.WriteString(` || ' `)
		where.Build(db.Statement)
		_, _ = db.Statement.WriteString(` AND '`)
	}
}

// appendCustomSchema writes the schema prefix when the caller addressed the
// table as schema.table.
func appendCustomSchema(schemaName string, db *gorm.DB) {
	if len(schemaName) > 0 {
		db.Statement.WriteQuoted(schemaName)
		_, _ = db.Statement.WriteString(".")
	}
}

// splitTableName splits a possible schema.table reference into its parts,
// trimming surrounding double quotes.
func splitTableName(tableName string) (schemaName, table string) {
	if strings.Contains(tableName, ".") {
		pattern := strings.Split(tableName, ".")
		if len(pattern) == 2 {
			schemaName = strings.Trim(pattern[0], `"`)
			table = strings.Trim(pattern[1], `"`)
			return schemaName, table
		}
	}
	return "", strings.Trim(tableName, `"`)
}

// getTableName prefers the raw table expression so db.Table("schema.t") and
// db.Table(gorm.Expr(...)) keep working.
func getTableName(stmt *gorm.Statement) string {
	if stmt.TableExpr != nil {
		return stmt.TableExpr.SQL
	}
	return stmt.Table
}

// appendVariableName builds the PL/SQL variable name for a column and row
// index; the suffix keeps rows and keywords apart.
func appendVariableName(columnName string, rowIndex string) string {
	return strings.ToUpper(columnName) + "_" + rowIndex
}

// BindVar appends bind values to stmt.Vars, flattening rows, clause
// expressions and driver.Valuer implementations like gorm's AddVar does.
func BindVar(stmt *gorm.Statement, vars ...interface{}) {
	for _, v := range vars {
		switch v := v.(type) {
		case sql.NamedArg:
			stmt.Vars = append(stmt.Vars, v.Value)
		case clause.Column, clause.Table:
			continue
		case gorm.Valuer:
			reflectValue := reflect.ValueOf(v)
			if reflectValue.Kind() == reflect.Pointer && reflectValue.IsNil() {
				BindVar(stmt, nil)
			} else {
				BindVar(stmt, v.GormValue(stmt.Context, stmt.DB))
			}
		case clause.Interface:
			c := clause.Clause{Name: v.Name()}
			v.MergeClause(&c)
			c.Build(stmt)
		case clause.Expression:
			v.Build(stmt)
		case driver.Valuer:
			stmt.Vars = append(stmt.Vars, v)
		case []byte:
			stmt.Vars = append(stmt.Vars, v)
		case []interface{}:
			if len(v) > 0 {
				BindVar(stmt, v...)
			} else {
				BindVar(stmt, nil)
			}
		default:
			switch rv := reflect.ValueOf(v); rv.Kind() {
			case reflect.Slice, reflect.Array:
				if rv.Len() == 0 {
					BindVar(stmt, nil)
				} else if rv.Type().Elem() == reflect.TypeOf(uint8(0)) {
					stmt.Vars = append(stmt.Vars, v)
				} else {
					for i := 0; i < rv.Len(); i++ {
						BindVar(stmt, rv.Index(i).Interface())
					}
				}
			default:
				stmt.Vars = append(stmt.Vars, v)
			}
		}
	}
}
