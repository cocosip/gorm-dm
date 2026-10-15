# gorm-dm

A [GORM V2](https://gorm.io) driver for the DaMeng database (DM8). **Pure Go, no CGO.**

It is modeled after the official DaMeng `gorm_v2_dialect` and keeps its public configuration API, but runs on the open-source pure-Go `database/sql` driver [gitee.com/chunanyong/dm](https://gitee.com/chunanyong/dm) and fixes several correctness issues against recent GORM releases (v1.31.x). Review the upsert semantics below when switching dialects.

## Features

- Pure Go; cross-compiles with `CGO_ENABLED=0`
- AutoMigrate based on the DaMeng system catalog (`SYS.SYSOBJECTS`/`SYSCOLUMNS`/`SYSCONS`/`SYSINDEXES`), including qualified schema lookups and catalog error propagation
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

Requires Go 1.26+; depends on `gorm.io/gorm` (v1.31.x) and `gitee.com/chunanyong/dm` v1.8.23.

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
| `GormMode` | DM upsert generation strategy: `GormModeDM`=0 / `GormModeMySQL`=1 / `GormModePostgres`=2 |

The server's `COMPATIBLE_MODE`, the driver's DSN `compatibleMode`, and this dialect's `GormMode` are separate settings. `GormMode` does not configure or detect either of the other settings. Every strategy emits DM SQL or DM procedural blocks, rather than sending native MySQL/PostgreSQL SQL to a compatible server. DM compatibility modes do not guarantee full support for another database's syntax. Runtime behavior across server/driver mode combinations has not been verified; system catalog access alone does not establish compatibility.

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

- Under `GormModeDM`, an update without conflict columns degrades to a plain INSERT. If the sole target is an omitted, generated primary key, a plain INSERT is also used. Otherwise every MERGE target column must be present in the insert values. MERGE omits assignments to ON columns because DM disallows updating them; it honors the update `Where` predicate.
- `DoNothing` suppresses duplicate-key errors. With explicit target columns, DM mode uses MERGE and PostgreSQL strategy validates the violated key. Without a target, all strategies use a DM procedural block to ignore duplicate keys.
- `GormModeMySQL`/`GormModePostgres` use a DM procedural block: each row is inserted via `EXECUTE IMMEDIATE`; a `DUP_VAL_ON_INDEX` exception falls back to an UPDATE built from the violated index in DM's system catalogs. Update expressions and `Where` values are bound separately from input values. An unresolvable violated key or a key missing from the input raises an error.
- For MySQL-style update blocks, `RowsAffected` retains the dialect's convention of reporting the number of input rows; this is not native MySQL's affected-row calculation. Plain INSERT uses the driver's count. Other blocks, including `DoNothing`, report the driver's count, which may not be the number of inserted rows.
- Conflict blocks never use a table-wide latest ID or `IDENT_CURRENT` for backfill. DM/PostgreSQL strategies may retrieve generated IDs using supplied, unchanged, non-NULL literal conflict keys. MySQL-style updates, `DoNothing`, absent keys, expressions, and changed conflict keys leave generated IDs unset. Use an application key to query the result when backfill is unavailable.
- Partial-index conflict targets (`TargetWhere`) return an explicit unsupported error. Named constraints (`OnConstraint`) require `GormModePostgres`.

`AutoMigrate` returns catalog lookup, scan, and iteration errors instead of treating them as missing objects and issuing DDL. The bool-only `HasTable`/`HasColumn`/`HasIndex`/`HasConstraint` APIs return false on failure and record the error on the migrator's DB. Migration detects both adding and removing a model's `not null` tag.

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

On case-sensitive DM databases, unquoted names normally resolve as uppercase while double-quoted names preserve case. Check the database's identifier case settings and the actual catalog names. If your application mixes GORM with raw SQL written against unquoted identifiers (e.g. casbin's default DDL), the optional upper-casing naming strategy can align generated names:

```go
db, err := gorm.Open(dm.Open(dsn), &gorm.Config{
    NamingStrategy: dm.NamingStrategy{
        NamingStrategy: schema.NamingStrategy{IdentifierMaxLength: 64},
    },
})
```

It is disabled by default — the dialect itself always double-quotes identifiers and preserves their case, matching the official dialect.

The strategy only affects names generated through GORM's Namer. Explicit `TableName()` results, `gorm:"column:..."` tags, raw SQL, and names embedded by generated query code must match the actual database names separately. Switching a compatibility mode does not enable this strategy or rename existing objects; use the same naming rules for migration and runtime queries.

## Migrating from the official gorm_v2_dialect

1. Change `import "dm"` to `import dm "github.com/cocosip/gorm-dm"`.
2. The API (`Open`/`New`/`Config` fields/`GormMode` values) is unchanged. Review naming, conflict-target validation, ID backfill, and error handling against the semantics above.

Improvements over the official dialect:

- Pure-Go open-source driver underneath; no proprietary DaMeng driver package to install
- DaMeng error checks via `errors.As` (the official raw type assertion panics on non-DaMeng errors)
- Adapted to GORM v1.31.x APIs (e.g. `GuessConstraintInterfaceAndTable`)
- AutoMigrate returns catalog failures; bool-only metadata APIs record failures on their DB; `HasColumn` accepts Go field names
- Index/column migrator APIs accept plain table names without panicking
- Full godoc and unit tests (the official dialect ships a single QuoteTo test)

## Testing

```bash
# Unit tests (no database required)
go test ./...

# Integration tests (require a real DaMeng instance)
DM_TEST_DSN="dm://SYSDBA:SYSDBA001@localhost:5236?schema=TEST" go test -v -run Integration
```

## CI and releasing

CI builds with CGO disabled, vets, and runs unit tests with the race detector
on Linux, Windows, and macOS using Go 1.27. It also verifies module
checksums and runs golangci-lint v2.14.0. Linux coverage is retained as
a workflow artifact for 14 days. `DM_TEST_DSN` is not configured in hosted CI,
so integration tests are skipped; CI does not validate real DM compatibility.

CodeQL analyzes Go code on pushes, pull requests, and a weekly schedule.
Dependabot checks Go modules and GitHub Actions weekly and groups update PRs
by ecosystem.

Pushing a `v*` tag runs the same CI checks before publishing a GitHub Release
with GitHub-generated release notes. Tags containing `-`, such as
`v1.0.0-rc.1`, are marked as prereleases:

```bash
git tag v1.0.0
git push origin v1.0.0
```

## License

[MIT](LICENSE)
