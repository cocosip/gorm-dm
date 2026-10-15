package dm

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/callbacks"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

const excludedTable = "excluded"

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
	var values clause.Values
	var hasConflict, merge, procedural bool
	var rowCount int64
	if db.Statement.SQL.String() == "" {
		values = callbacks.ConvertToCreateValues(db.Statement)
		c := db.Statement.Clauses["ON CONFLICT"]
		onConflict, hasConflict = c.Expression.(clause.OnConflict)
		if len(onConflict.TargetWhere.Exprs) > 0 {
			_ = db.AddError(fmt.Errorf("dm: partial-index conflict targets are not supported"))
			return
		}
		if db.Error != nil {
			return
		}

		gormMode := dialectorGormMode(db)
		if hasConflict && (gormMode == GormModeMySQL || gormMode == GormModePostgres ||
			(onConflict.DoNothing && len(onConflict.Columns) == 0)) {
			defer applyIdentityInsert(db)()
			Upsert(gormMode, db, onConflict, values)
			rowCount = int64(len(values.Values))
			procedural = onConflict.DoNothing || onConflict.UpdateAll || len(onConflict.DoUpdates) > 0
		} else {
			if onConflict.OnConstraint != "" {
				_ = db.AddError(fmt.Errorf("dm: named conflict constraints require GormModePostgres"))
				return
			}
			// MERGE INTO requires explicit conflict columns; without them the
			// statement degrades to a plain INSERT.
			if hasConflict && len(onConflict.Columns) == 0 {
				hasConflict = false
			}
			if hasConflict && missingIdentityTarget(db, onConflict, values) {
				hasConflict = false
			}

			if hasConflict {
				defer applyIdentityInsert(db)()
				MergeCreate(db, onConflict, values)
				merge = true
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

	if merge || procedural {
		execConflict(db, onConflict, values, procedural && dialectorGormMode(db) == GormModeMySQL && !onConflict.DoNothing, rowCount)
		return
	}

	execInsert(db)
}

// An omitted generated primary key cannot conflict; use INSERT so its ID can
// come from LastInsertId instead of referring to a nonexistent source column.
func missingIdentityTarget(db *gorm.DB, conflict clause.OnConflict, values clause.Values) bool {
	if db.Statement.Schema == nil || len(conflict.Columns) != 1 {
		return false
	}
	field := db.Statement.Schema.PrioritizedPrimaryField
	if field == nil || !field.AutoIncrement || conflict.Columns[0].Name != field.DBName {
		return false
	}
	for _, column := range values.Columns {
		if column.Name == field.DBName {
			return false
		}
	}
	return true
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
		for i := 0; i < db.Statement.ReflectValue.Len(); i++ {
			obj := db.Statement.ReflectValue.Index(i)
			if reflect.Indirect(obj).Kind() == reflect.Struct {
				_, isZero := field.ValueOf(db.Statement.Context, obj)
				setIdentityInsert = setIdentityInsert || !isZero
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
	_, err := db.Statement.ConnPool.ExecContext(db.Statement.Context, db.Statement.SQL.String())
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
	_, err := db.Statement.ConnPool.ExecContext(db.Statement.Context, db.Statement.SQL.String())
	_ = db.AddError(err)
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
			_, _ = db.Statement.WriteString(" DEFAULT VALUES")
		}
	}
}

// MergeCreate builds a DaMeng MERGE INTO statement for GormModeDM upserts.
func MergeCreate(db *gorm.DB, onConflict clause.OnConflict, values clause.Values) {
	if len(onConflict.TargetWhere.Exprs) > 0 || onConflict.OnConstraint != "" || len(onConflict.Columns) == 0 {
		_ = db.AddError(fmt.Errorf("dm: MERGE requires conflict columns and does not support named or partial-index targets"))
		return
	}
	for _, target := range onConflict.Columns {
		found := false
		for _, column := range values.Columns {
			found = found || column.Name == target.Name
		}
		if !found {
			_ = db.AddError(fmt.Errorf("dm: conflict column %q has no insert value", target.Name))
			return
		}
	}
	db.Statement.SQL.Reset()
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
			Value:  clause.Column{Table: excludedTable, Name: col.Name},
		})
	}
	where.Build(db.Statement)

	writeMergeUpdate(db, onConflict)
	writeMergeInsert(db, values)
}

// writeMergeUpdate emits WHEN MATCHED THEN UPDATE SET, dropping assignments
// on columns used in the ON condition (DaMeng error -4064).
func writeMergeUpdate(db *gorm.DB, onConflict clause.OnConflict) {
	if onConflict.DoNothing || len(onConflict.DoUpdates) == 0 {
		return
	}
	withoutOnColumns := make([]clause.Assignment, 0, len(onConflict.DoUpdates))
outer:
	for _, assignment := range onConflict.DoUpdates {
		for _, col := range onConflict.Columns {
			if assignment.Column.Name == col.Name {
				continue outer
			}
		}
		withoutOnColumns = append(withoutOnColumns, assignment)
	}
	onConflict.DoUpdates = clause.Set(withoutOnColumns)
	if len(onConflict.DoUpdates) > 0 {
		_, _ = db.Statement.WriteString(" WHEN MATCHED THEN UPDATE SET ")
		onConflict.DoUpdates.Build(db.Statement)
		if len(onConflict.Where.Exprs) > 0 {
			_, _ = db.Statement.WriteString(" WHERE ")
			onConflict.Where.Build(db.Statement)
		}
	}
}

// writeMergeInsert preserves explicitly supplied identity values.
func writeMergeInsert(db *gorm.DB, values clause.Values) {
	_, _ = db.Statement.WriteString(" WHEN NOT MATCHED THEN INSERT (")

	written := false
	for _, column := range values.Columns {
		if written {
			_ = db.Statement.WriteByte(',')
		}
		written = true
		db.Statement.WriteQuoted(column.Name)
	}

	_, _ = db.Statement.WriteString(") VALUES (")

	written = false
	for _, column := range values.Columns {
		if written {
			_ = db.Statement.WriteByte(',')
		}
		written = true
		db.Statement.WriteQuoted(clause.Column{
			Table: excludedTable,
			Name:  column.Name,
		})
	}

	_, _ = db.Statement.WriteString(")")
	_, _ = db.Statement.WriteString(";")
}

// Upsert builds a PL/SQL block emulating MySQL's ON DUPLICATE KEY UPDATE
// (GormModeMySQL) or PostgreSQL's ON CONFLICT DO UPDATE (GormModePostgres):
// every row is inserted with EXECUTE IMMEDIATE and a DUP_VAL_ON_INDEX
// exception falls back to a dynamically built UPDATE on the indexed columns.
func Upsert(gormMode int, db *gorm.DB, onConflict clause.OnConflict, values clause.Values) {
	if len(onConflict.TargetWhere.Exprs) > 0 {
		_ = db.AddError(fmt.Errorf("dm: partial-index conflict targets are not supported"))
		return
	}
	if onConflict.OnConstraint != "" && gormMode != GormModePostgres {
		_ = db.AddError(fmt.Errorf("dm: named conflict constraints require GormModePostgres"))
		return
	}
	if onConflict.UpdateAll && !onConflict.DoNothing && len(onConflict.DoUpdates) == 0 {
		_ = db.AddError(fmt.Errorf("dm: UpdateAll must be expanded into assignments by the GORM create callback"))
		return
	}
	if !onConflict.DoNothing && !onConflict.UpdateAll && len(onConflict.DoUpdates) == 0 {
		// Nothing to update on conflict: a plain INSERT is enough.
		normalInsert(db, values)
		return
	}

	schemaName, tableName := splitTableName(getTableName(db.Statement))
	rowCount := len(values.Values)

	db.Statement.SQL.Reset()
	writeUpsertDeclare(db, onConflict, values, gormMode, schemaName, tableName, rowCount)
	updates := writeUpsertUpdateDecl(db, onConflict, values, schemaName, tableName, rowCount)
	_, _ = db.Statement.WriteString(` BEGIN `)
	for i := 0; i < rowCount; i++ {
		writeUpsertRowBlock(db, onConflict, values, gormMode, schemaName, tableName, i, updates[i])
	}
	_, _ = db.Statement.WriteString("END;")
}

// writeUpsertDeclare writes typed input variables and conflict bookkeeping.
func writeUpsertDeclare(db *gorm.DB, onConflict clause.OnConflict, values clause.Values,
	gormMode int, schemaName, tableName string, rowCount int) {
	_, _ = db.Statement.WriteString("/*DMGORM-UPSERT*/DECLARE ")
	for i := 0; i < rowCount; i++ {
		for j, columnName := range values.Columns {
			_, _ = db.Statement.WriteString(appendVariableName(j, strconv.Itoa(i)) + " ")
			appendCustomSchema(schemaName, db)
			db.Statement.WriteQuoted(tableName)
			_ = db.Statement.WriteByte('.')
			db.Statement.WriteQuoted(columnName.Name)
			_, _ = db.Statement.WriteString("%TYPE := ")
			db.Statement.AddVar(db.Statement, values.Values[i][j])
			_ = db.Statement.WriteByte(';')
		}
	}
	_, _ = db.Statement.WriteString(" TYPE emp_tab IS TABLE OF VARCHAR2; ")
	if gormMode == GormModePostgres && len(onConflict.Columns) > 0 {
		_, _ = db.Statement.WriteString(` v_columns emp_tab := emp_tab(`)
		for i, v := range onConflict.Columns {
			_, _ = db.Statement.WriteString(" " + sqlString(v.Name) + " ")
			if i < len(onConflict.Columns)-1 {
				_, _ = db.Statement.WriteString(", ")
			}
		}
		_, _ = db.Statement.WriteString(`); v_diff1 emp_tab; v_diff2 emp_tab; `)
	}
}

// upsertBindDialector names placeholders inside dynamic SQL; their values are
// bound by EXECUTE IMMEDIATE, separately from the outer statement's parameters.
type upsertBindDialector struct {
	gorm.Dialector
	prefix string
}

func (d upsertBindDialector) BindVarTo(w clause.Writer, stmt *gorm.Statement, _ interface{}) {
	_, _ = w.WriteString(":" + d.prefix + strconv.Itoa(len(stmt.Vars)))
}

type upsertUpdate struct {
	vars  []interface{}
	where string
}

func sqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// Build update expressions in their row context, not in the outer DECLARE.
func writeUpsertUpdateDecl(db *gorm.DB, onConflict clause.OnConflict, values clause.Values,
	schemaName, tableName string, rowCount int) []upsertUpdate {
	updates := make([]upsertUpdate, rowCount)
	for i := 0; i < rowCount; i++ {
		rowIndex := strconv.Itoa(i)
		_, _ = db.Statement.WriteString("v_emp_data" + rowIndex + " emp_tab; v_index_name" + rowIndex + " VARCHAR2(32767); ")
		if onConflict.DoNothing {
			continue
		}

		config := *db.Config
		config.Dialector = upsertBindDialector{Dialector: db.Dialector, prefix: "dm_update_" + rowIndex + "_"}
		tx := &gorm.DB{Config: &config}
		stmt := &gorm.Statement{DB: tx, Context: db.Statement.Context, Table: db.Statement.Table, Schema: db.Statement.Schema}
		tx.Statement = stmt
		_, _ = stmt.WriteString("DECLARE ")
		for j, col := range values.Columns {
			_, _ = stmt.WriteString(appendVariableName(j, rowIndex) + "X ")
			appendCustomSchema(schemaName, tx)
			stmt.WriteQuoted(tableName)
			_ = stmt.WriteByte('.')
			stmt.WriteQuoted(col.Name)
			_, _ = stmt.WriteString("%TYPE := :" + appendVariableName(j, rowIndex) + "; ")
		}
		_, _ = stmt.WriteString("BEGIN UPDATE ")
		appendCustomSchema(schemaName, tx)
		stmt.WriteQuoted(tableName)
		_, _ = stmt.WriteString(" SET ")
		for j, assignment := range onConflict.DoUpdates {
			if j > 0 {
				_, _ = stmt.WriteString(", ")
			}
			stmt.WriteQuoted(assignment.Column)
			_, _ = stmt.WriteString(" = ")
			if col, ok := assignment.Value.(clause.Column); ok && col.Table == excludedTable {
				found := false
				for k, input := range values.Columns {
					if input.Name == col.Name {
						found = true
						_, _ = stmt.WriteString(appendVariableName(k, rowIndex) + "X")
						break
					}
				}
				if !found {
					_ = db.AddError(fmt.Errorf("dm: update source column %q has no insert value", col.Name))
				}
			} else {
				stmt.AddVar(stmt, assignment.Value)
			}
		}
		_, _ = stmt.WriteString(" WHERE ")
		_, _ = db.Statement.WriteString("v_sql_update" + rowIndex + " VARCHAR2(32767) := " + sqlString(stmt.SQL.String()) + "; ")
		stmt.SQL.Reset()
		if len(onConflict.Where.Exprs) > 0 {
			onConflict.Where.Build(stmt)
			updates[i].where = stmt.SQL.String()
		}
		updates[i].vars = stmt.Vars
		if stmt.Error != nil {
			_ = db.AddError(stmt.Error)
		}
		_, _ = db.Statement.WriteString("v_condition" + rowIndex + " VARCHAR2(32767) := ''; ")
	}
	return updates
}

// writeUpsertRowBlock writes the per-row BEGIN ... EXCEPTION ... END block:
// the EXECUTE IMMEDIATE INSERT and, on DUP_VAL_ON_INDEX, the index lookup,
// the WHERE condition assembly and the fallback UPDATE execution.
func writeUpsertRowBlock(db *gorm.DB, onConflict clause.OnConflict, values clause.Values,
	gormMode int, schemaName, tableName string, rowIdx int, update upsertUpdate) {
	rowIndex := strconv.Itoa(rowIdx)

	_, _ = db.Statement.WriteString(` BEGIN `)
	insertStart := db.Statement.SQL.Len()
	_, _ = db.Statement.WriteString(`INSERT INTO `)
	appendCustomSchema(schemaName, db)
	db.Statement.WriteQuoted(tableName)
	if len(values.Columns) == 0 {
		_, _ = db.Statement.WriteString(" DEFAULT VALUES;")
	} else {
		_, _ = db.Statement.WriteString(" (")
		for idx := range values.Columns {
			if idx > 0 {
				_, _ = db.Statement.WriteString(", ")
			}
			db.Statement.WriteQuoted(values.Columns[idx].Name)
		}
		_, _ = db.Statement.WriteString(") VALUES(")
		for idx := range values.Columns {
			if idx > 0 {
				_, _ = db.Statement.WriteString(", ")
			}
			_ = db.Statement.WriteByte(':')
			_, _ = db.Statement.WriteString(appendVariableName(idx, rowIndex))
		}
		_, _ = db.Statement.WriteString(");")
	}
	fullSQL := db.Statement.SQL.String()
	insertSQL := fullSQL[insertStart:]
	db.Statement.SQL.Reset()
	_, _ = db.Statement.WriteString(fullSQL[:insertStart])
	_, _ = db.Statement.WriteString(" EXECUTE IMMEDIATE " + sqlString(insertSQL))
	if len(values.Columns) > 0 {
		_, _ = db.Statement.WriteString(" USING ")
	}
	for idx := range values.Columns {
		if idx > 0 {
			_, _ = db.Statement.WriteString(", ")
		}
		_, _ = db.Statement.WriteString(appendVariableName(idx, rowIndex))
	}
	_, _ = db.Statement.WriteString(`; `)

	_, _ = db.Statement.WriteString(`EXCEPTION WHEN DUP_VAL_ON_INDEX THEN `)
	if !onConflict.DoNothing || (gormMode == GormModePostgres && (len(onConflict.Columns) > 0 || onConflict.OnConstraint != "")) {
		writeUpsertIndexLookup(db, onConflict, gormMode, schemaName, tableName, rowIndex)
		_, _ = db.Statement.WriteString("IF v_emp_data" + rowIndex + ".COUNT = 0 THEN RAISE; END IF; ")
	}
	if onConflict.DoNothing {
		_, _ = db.Statement.WriteString("NULL; WHEN OTHERS THEN RAISE; END; ")
		return
	}
	writeUpsertConditionLoop(db, values, rowIndex, update.where)
	writeUpsertFallbackUpdate(db, values, rowIndex, update.vars)
}

// writeUpsertIndexLookup resolves the violated key through DM's own catalogs.
func writeUpsertIndexLookup(db *gorm.DB, onConflict clause.OnConflict,
	gormMode int, schemaName, tableName, rowIndex string) {
	_, _ = db.Statement.WriteString(`v_index_name` + rowIndex +
		` := (SELECT SUBSTR(SQLERRM, INSTR(SQLERRM, '[', -1, 1) + 1, INSTR(SQLERRM, ']', -1, 1) - INSTR(SQLERRM, '[', -1, 1) - 1));
 SELECT DISTINCT COLS.NAME BULK COLLECT INTO v_emp_data` + rowIndex + `
 FROM SYS.SYSOBJECTS TABS
 JOIN SYS.SYSCOLUMNS COLS ON COLS.ID = TABS.ID
 JOIN SYS.SYSOBJECTS IDX ON IDX.PID = TABS.ID AND IDX.SUBTYPE$ = 'INDEX'
 JOIN SYS.SYSINDEXES INDS ON INDS.ID = IDX.ID
 LEFT JOIN SYS.SYSCONS CONS ON CONS.INDEXID = INDS.ID
 LEFT JOIN SYS.SYSOBJECTS CON_OBJ ON CON_OBJ.ID = CONS.ID
 WHERE INDS.ISUNIQUE = 'Y'
 AND SF_COL_IS_IDX_KEY(INDS.KEYNUM, INDS.KEYINFO, COLS.COLID) = 1
 AND (IDX.NAME = v_index_name` + rowIndex + ` OR CON_OBJ.NAME = v_index_name` + rowIndex + `)
 AND TABS.NAME = ` + sqlString(tableName) + ` AND TABS.SCHID = `)
	if schemaName == "" {
		_, _ = db.Statement.WriteString("CURRENT_SCHID()")
	} else {
		_, _ = db.Statement.WriteString("(SELECT ID FROM SYS.SYSOBJECTS WHERE TYPE$ = 'SCH' AND NAME = " + sqlString(schemaName) + ")")
	}
	if gormMode == GormModePostgres && onConflict.OnConstraint != "" {
		_, _ = db.Statement.WriteString(" AND CON_OBJ.NAME = " + sqlString(onConflict.OnConstraint))
	}
	_, _ = db.Statement.WriteString("; ")
	if gormMode == GormModePostgres && len(onConflict.Columns) > 0 {
		_, _ = db.Statement.WriteString(`BEGIN v_diff1 := v_emp_data` + rowIndex + ` multiset except v_columns; v_diff2 := v_columns multiset except v_emp_data` + rowIndex + `; `)
		_, _ = db.Statement.WriteString(`if v_diff1.COUNT > 0 OR v_diff2.COUNT > 0 THEN RAISE; END IF; END;`)
	}
}

// The duplicate index determines the row; the caller's predicate may only
// restrict that UPDATE. A partial-index target is rejected before generation.
func writeUpsertConditionLoop(db *gorm.DB, values clause.Values, rowIndex, predicate string) {
	_, _ = db.Statement.WriteString("FOR i IN 1..v_emp_data" + rowIndex + ".COUNT LOOP ")
	for j, column := range values.Columns {
		if j > 0 {
			_, _ = db.Statement.WriteString("ELS")
		}
		_, _ = db.Statement.WriteString("IF v_emp_data" + rowIndex + "(i) = " + sqlString(column.Name) + " THEN ")
		condition := db.Statement.Quote(column) + " = " + appendVariableName(j, rowIndex) + "X AND "
		_, _ = db.Statement.WriteString("v_condition" + rowIndex + " := v_condition" + rowIndex + " || " + sqlString(condition) + "; ")
	}
	_, _ = db.Statement.WriteString("ELSE RAISE; END IF; END LOOP; ")
	if predicate != "" {
		_, _ = db.Statement.WriteString("v_condition" + rowIndex + " := v_condition" + rowIndex + " || " + sqlString("("+predicate+") AND ") + "; ")
	}
	_, _ = db.Statement.WriteString("v_sql_update" + rowIndex + " := v_sql_update" + rowIndex + " || v_condition" + rowIndex + " || '1 = 1; END;'; EXECUTE IMMEDIATE v_sql_update" + rowIndex + " USING ")
}

func writeUpsertFallbackUpdate(db *gorm.DB, values clause.Values, rowIndex string, vars []interface{}) {
	for i := range values.Columns {
		if i > 0 {
			_, _ = db.Statement.WriteString(", ")
		}
		_, _ = db.Statement.WriteString(appendVariableName(i, rowIndex))
	}
	for _, value := range vars {
		_, _ = db.Statement.WriteString(", ")
		db.Statement.AddVar(db.Statement, value)
	}
	_, _ = db.Statement.WriteString("; WHEN OTHERS THEN RAISE; END; ")
}

// Conflict statements do not have a reliable last-insert identity. Resolve
// zero primary keys only through unchanged, supplied conflict-key values.
func execConflict(db *gorm.DB, conflict clause.OnConflict, values clause.Values, countInputs bool, rowCount int64) {
	result, err := db.Statement.ConnPool.ExecContext(db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...)
	if db.AddError(err) != nil {
		return
	}
	db.RowsAffected, err = result.RowsAffected()
	if db.AddError(err) != nil {
		return
	}
	if countInputs {
		db.RowsAffected = rowCount
	}
	// MySQL-style conflict handling may match a different unique key.
	if countInputs || conflict.DoNothing || db.Statement.Schema == nil || len(conflict.Columns) == 0 {
		return
	}
	field := db.Statement.Schema.PrioritizedPrimaryField
	if field == nil || !field.AutoIncrement {
		return
	}
	for _, assignment := range conflict.DoUpdates {
		for _, key := range conflict.Columns {
			if assignment.Column.Name == key.Name {
				col, ok := assignment.Value.(clause.Column)
				if !ok || col.Table != excludedTable || col.Name != key.Name {
					return
				}
			}
		}
	}
	backfillConflictIDs(db, field, conflict, values)
}

func backfillConflictIDs(db *gorm.DB, field *schema.Field, conflict clause.OnConflict, values clause.Values) {
	for i, row := range values.Values {
		record := db.Statement.ReflectValue
		if record.Kind() == reflect.Slice || record.Kind() == reflect.Array {
			record = record.Index(i)
		}
		if reflect.Indirect(record).Kind() != reflect.Struct {
			return
		}
		if _, zero := field.ValueOf(db.Statement.Context, record); !zero {
			continue
		}
		where := clause.Where{}
		for _, key := range conflict.Columns {
			found := false
			for j, column := range values.Columns {
				if column.Name != key.Name {
					continue
				}
				value := row[j]
				if value == nil {
					return
				}
				switch value.(type) {
				case clause.Expression, gorm.Valuer, driver.Valuer:
					return
				}
				rv := reflect.ValueOf(value)
				if rv.Kind() == reflect.Pointer && rv.IsNil() {
					return
				}
				where.Exprs = append(where.Exprs, clause.Eq{Column: clause.Column{Name: key.Name}, Value: value})
				found = true
				break
			}
			if !found {
				return
			}
		}
		stmt := &gorm.Statement{DB: db}
		_, _ = stmt.WriteString("SELECT ")
		stmt.WriteQuoted(field.DBName)
		_, _ = stmt.WriteString(" FROM ")
		stmt.WriteQuoted(getTableName(db.Statement))
		_, _ = stmt.WriteString(" WHERE ")
		where.Build(stmt)
		rows, queryErr := db.Statement.ConnPool.QueryContext(db.Statement.Context, stmt.SQL.String(), stmt.Vars...)
		if db.AddError(queryErr) != nil {
			return
		}
		var id int64
		var err error
		found := rows.Next()
		if found {
			err = rows.Scan(&id)
		}
		if err == nil && rows.Next() {
			err = fmt.Errorf("dm: conflict key matches multiple rows")
		}
		if err == nil {
			err = rows.Err()
		}
		closeErr := rows.Close()
		if db.AddError(err) != nil {
			return
		}
		if db.AddError(closeErr) != nil {
			return
		}
		if found && id > 0 {
			if db.AddError(field.Set(db.Statement.Context, record, id)) != nil {
				return
			}
		}
	}
}

// execInsert executes a plain INSERT and backfills generated primary keys.
func execInsert(db *gorm.DB) {
	result, err := db.Statement.ConnPool.ExecContext(db.Statement.Context, db.Statement.SQL.String(), db.Statement.Vars...)
	if db.AddError(err) != nil {
		return
	}
	db.RowsAffected, err = result.RowsAffected()
	if db.AddError(err) != nil {
		return
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

// Ordinal variables also work for mixed-case and non-ASCII column names.
func appendVariableName(columnIndex int, rowIndex string) string {
	return "dm_input_" + rowIndex + "_" + strconv.Itoa(columnIndex)
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
