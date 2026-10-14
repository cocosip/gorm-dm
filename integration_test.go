package dm

import (
	"os"
	"testing"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// These tests run only against a real DaMeng server. Set DM_TEST_DSN to
// enable them, e.g.:
//
//	DM_TEST_DSN="dm://SYSDBA:SYSDBA001@localhost:5236?schema=TEST" go test -v -run Integration
func integrationDB(t *testing.T, gormMode int) *gorm.DB {
	t.Helper()

	dsn := os.Getenv("DM_TEST_DSN")
	if dsn == "" {
		t.Skip("DM_TEST_DSN not set; skipping integration test")
	}

	db, err := gorm.Open(New(Config{DSN: dsn, GormMode: gormMode}), &gorm.Config{})
	if err != nil {
		t.Fatalf("open dm: %v", err)
	}
	return db
}

type integUser struct {
	ID    uint64 `gorm:"primaryKey;autoIncrement"`
	Name  string `gorm:"size:64;uniqueIndex"`
	Age   int32
	Email string `gorm:"size:128;comment:user email"`
}

func (integUser) TableName() string { return "dm_gorm_it_users" }

func TestIntegrationAutoMigrateAndCRUD(t *testing.T) {
	db := integrationDB(t, GormModeDM)

	if err := db.Migrator().DropTable(&integUser{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if err := db.AutoMigrate(&integUser{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	if !db.Migrator().HasTable(&integUser{}) {
		t.Fatal("HasTable = false after AutoMigrate")
	}

	user := integUser{Name: "alice", Age: 30, Email: "a@example.com"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if user.ID == 0 {
		t.Fatal("auto-increment id not backfilled")
	}

	var got integUser
	if err := db.First(&got, user.ID).Error; err != nil {
		t.Fatalf("first: %v", err)
	}
	if got.Name != "alice" || got.Age != 30 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if err := db.Model(&got).Update(colAge, 31).Error; err != nil {
		t.Fatalf("update: %v", err)
	}
	if res := db.Delete(&got); res.Error != nil || res.RowsAffected != 1 {
		t.Fatalf("delete: error=%v affected=%d", res.Error, res.RowsAffected)
	}

	indexes, err := db.Migrator().GetIndexes(&integUser{})
	if err != nil {
		t.Fatalf("get indexes: %v", err)
	}
	if len(indexes) == 0 {
		t.Fatal("no indexes reported")
	}

	if err := db.Migrator().DropTable(&integUser{}); err != nil {
		t.Fatalf("cleanup drop table: %v", err)
	}
}

func TestIntegrationUpsertMergeMode(t *testing.T) {
	db := integrationDB(t, GormModeDM)

	if err := db.Migrator().DropTable(&integUser{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if err := db.AutoMigrate(&integUser{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	defer func() { _ = db.Migrator().DropTable(&integUser{}) }()

	first := integUser{Name: "bob", Age: 1, Email: "b@example.com"}
	if err := db.Create(&first).Error; err != nil {
		t.Fatalf("seed create: %v", err)
	}

	// Same unique name but a fresh identity value: the MERGE should update
	// the existing row instead of inserting a duplicate.
	upsert := integUser{Name: "bob", Age: 2, Email: "b2@example.com"}
	err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: colName}},
		DoUpdates: clause.AssignmentColumns([]string{colAge, "email"}),
	}).Create(&upsert).Error
	if err != nil {
		t.Fatalf("merge upsert: %v", err)
	}

	var count int64
	if err := db.Model(&integUser{}).Where(colName + " = ?", "bob").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows named bob = %d, want 1 (upsert must not duplicate)", count)
	}

	var age int32
	if err := db.Model(&integUser{}).Where(colName + " = ?", "bob").Pluck(colAge, &age).Error; err != nil {
		t.Fatalf("pluck age: %v", err)
	}
	if age != 2 {
		t.Fatalf("age after upsert = %d, want 2", age)
	}
}

func TestIntegrationUpsertMySQLMode(t *testing.T) {
	db := integrationDB(t, GormModeMySQL)

	if err := db.Migrator().DropTable(&integUser{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if err := db.AutoMigrate(&integUser{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	defer func() { _ = db.Migrator().DropTable(&integUser{}) }()

	if err := db.Create(&integUser{Name: "carol", Age: 10, Email: "c@example.com"}).Error; err != nil {
		t.Fatalf("seed create: %v", err)
	}

	err := db.Clauses(clause.OnConflict{
		UpdateAll: true,
	}).Create(&integUser{Name: "carol", Age: 11, Email: "c2@example.com"}).Error
	if err != nil {
		t.Fatalf("mysql upsert: %v", err)
	}

	var count int64
	if err := db.Model(&integUser{}).Where(colName + " = ?", "carol").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows named carol = %d, want 1", count)
	}
}

func TestIntegrationUpsertPostgresMode(t *testing.T) {
	db := integrationDB(t, GormModePostgres)

	if err := db.Migrator().DropTable(&integUser{}); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if err := db.AutoMigrate(&integUser{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	defer func() { _ = db.Migrator().DropTable(&integUser{}) }()

	if err := db.Create(&integUser{Name: "dave", Age: 20, Email: "d@example.com"}).Error; err != nil {
		t.Fatalf("seed create: %v", err)
	}

	err := db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: colName}},
		DoUpdates: clause.AssignmentColumns([]string{colAge}),
	}).Create(&integUser{Name: "dave", Age: 21, Email: "d2@example.com"}).Error
	if err != nil {
		t.Fatalf("postgres upsert: %v", err)
	}

	var count int64
	if err := db.Model(&integUser{}).Where(colName + " = ?", "dave").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows named dave = %d, want 1", count)
	}
}
