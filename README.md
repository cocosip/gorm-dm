# gorm-dm

A [GORM V2](https://gorm.io) driver for the DaMeng database (DM8). **Pure Go, no CGO.**

It is modeled after the official DaMeng `gorm_v2_dialect` and keeps its API and behavioral semantics, but runs on the open-source pure-Go `database/sql` driver [gitee.com/chunanyong/dm](https://gitee.com/chunanyong/dm) and fixes several robustness issues of the official dialect against recent GORM releases (v1.31.x). Migrating from the official dialect only requires changing the import path.

## Features

- Pure Go; cross-compiles with `CGO_ENABLED=0`
- Full AutoMigrate support based on the DaMeng system catalog (`SYS.SYSOBJECTS`/`SYSCOLUMNS`/`SYSCONS`/`SYSINDEXES`), independent of the server compatibility mode
- Auto-increment primary keys: `IDENTITY(1,1)`, ID backfill after insert, and automatic `SET IDENTITY_INSERT ON/OFF` when explicit values are provided
- Three upsert strategies (`clause.OnConflict`):
  - `GormModeDM` (default): native DaMeng `MERGE INTO`
  - `GormModeMySQL`: emulates MySQL `ON DUPLICATE KEY UPDATE`
  - `GormModePostgres`: emulates PostgreSQL `ON CONFLICT DO UPDATE` (conflict column validation, constraint names, WHERE clauses)
- Column comments (`comment` tag → `COMMENT ON COLUMN`)
- Preserves identifier case like the official dialect; an optional upper-casing `NamingStrategy` is provided

## Installation

```bash
go get github.com/cocosip/gorm-dm
```

Requires Go 1.22+; depends on `gorm.io/gorm` (v1.31.x) and `gitee.com/chunanyong/dm` v1.8.23.

## Quick Start

```go
import (
    "github.com/cocosip/gorm-dm"
    "gorm.io/gorm"
)

db, err := gorm.Open(dm.Open("dm://SYSDBA:SYSDBA001@localhost:5236?schema=TEST"), &gorm.Config{})
```

Or with the full configuration:

```go
db, err := gorm.Open(dm.New(dm.Config{
    DSN:      "dm://SYSDBA:SYSDBA001@localhost:5236?schema=TEST",
    GormMode: dm.GormModeMySQL, // optional, defaults to GormModeDM
}), &gorm.Config{})
```

### Config

| Field | Description |
|---|---|
| `DriverName` | database/sql driver name, defaults to `"dm"` |
| `DSN` | connection string, `dm://user:password@host:port?schema=NAME&key=value`; see the [chunanyong/dm](https://gitee.com/chunanyong/dm) docs for supported parameters |
| `Conn` | inject an existing connection pool (e.g. `*sql.DB`); when set, DSN is ignored |
| `DefaultStringSize` | default VARCHAR size for string fields without a size tag; defaults to 0 (string columns that are not primary key / indexed / defaulted map to CLOB) |
| `GormMode` | upsert compatibility mode: `GormModeDM`=0 / `GormModeMySQL`=1 / `GormModePostgres`=2 |

## Upsert Semantics

```go
// GormModeDM: MERGE INTO (conflict columns are required; with UpdateAll
// GORM fills them with the primary key)
db.Clauses(clause.OnConflict{
    Columns:   []clause.Column{{Name: "name"}},
    DoUpdates: clause.AssignmentColumns([]string{"age"}),
}).Create(&user)

// GormModeMySQL: emulates ON DUPLICATE KEY UPDATE
db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&user)

// GormModePostgres: emulates ON CONFLICT (name) DO UPDATE
db.Clauses(clause.OnConflict{
    Columns:   []clause.Column{{Name: "name"}},
    DoUpdates: clause.AssignmentColumns([]string{"age"}),
}).Create(&user)
```

Notes:

- Under `GormModeDM`, an upsert without conflict columns (and without `UpdateAll`) degrades to a plain INSERT, because the `MERGE INTO` ON condition cannot be built.
- `GormModeMySQL`/`GormModePostgres` are implemented as a PL/SQL block: each row is inserted via `EXECUTE IMMEDIATE`; a `DUP_VAL_ON_INDEX` exception falls back to an UPDATE built dynamically from the violated index.
- Under `GormModeMySQL`, `RowsAffected` reports the number of processed rows (MySQL semantics).

## Type Mapping

| GORM type | DaMeng type |
|---|---|
| Bool | `BIT` |
| Int/Uint (8/16/32/64 bit) | `TINYINT` / `SMALLINT` / `INT` / `BIGINT` |
| Auto-increment | appends ` IDENTITY(1,1)` |
| Float (with precision) | `DECIMAL(p, s)` |
| Float | `DOUBLE` |
| String | `VARCHAR(n)`; no size and not PK/default/indexed → `CLOB`; size ≥32768 → `CLOB` |
| Time | `TIMESTAMP` (with precision → `TIMESTAMP(p)`) |
| Bytes | `VARBINARY(n)`; oversized → `BLOB` |

## Optional: upper-casing NamingStrategy

DaMeng folds unquoted identifiers to uppercase. If your application mixes GORM with raw SQL written against unquoted identifiers (e.g. casbin's default DDL), install the upper-casing naming strategy to keep both worlds consistent:

```go
db, err := gorm.Open(dm.Open(dsn), &gorm.Config{
    NamingStrategy: dm.NamingStrategy{
        NamingStrategy: schema.NamingStrategy{IdentifierMaxLength: 64},
    },
})
```

It is disabled by default — the dialect itself always double-quotes identifiers and preserves their case, matching the official dialect.

## Migrating from the official gorm_v2_dialect

1. Change `import "dm"` to `import dm "github.com/cocosip/gorm-dm"`.
2. The API (`Open`/`New`/`Config` fields/`GormMode` values) is unchanged; no other code changes are needed.

Improvements over the official dialect:

- Pure-Go open-source driver underneath; no proprietary DaMeng driver package to install
- DaMeng error checks via `errors.As` (the official raw type assertion panics on non-DaMeng errors)
- Adapted to GORM v1.31.x APIs (e.g. `GuessConstraintInterfaceAndTable`)
- Migrator catalog errors are no longer silently swallowed; `HasColumn` accepts Go field names
- Index/column migrator APIs accept plain table names without panicking
- Full godoc and unit tests (the official dialect ships a single QuoteTo test)

## Testing

```bash
# Unit tests (no database required)
go test ./...

# Integration tests (require a real DaMeng instance)
DM_TEST_DSN="dm://SYSDBA:SYSDBA001@localhost:5236?schema=TEST" go test -v -run Integration
```

## License

[MIT](LICENSE)
