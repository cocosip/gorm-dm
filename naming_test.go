package dm

import (
	"testing"

	"gorm.io/gorm/schema"
)

func TestNamingStrategyUppercasesIdentifiers(t *testing.T) {
	ns := NamingStrategy{NamingStrategy: schema.NamingStrategy{IdentifierMaxLength: 64}}

	tests := []struct {
		name string
		got  string
		want string
	}{
		{name: "table", got: ns.TableName("CasbinRule"), want: "CASBIN_RULES"},
		{name: "column", got: ns.ColumnName("", "LastLoginAt"), want: "LAST_LOGIN_AT"},
		{name: "initialism column", got: ns.ColumnName("", "V0"), want: "V0"},
		{name: "join table", got: ns.JoinTableName("userRole"), want: "USER_ROLES"},
		{name: "index", got: ns.IndexName("casbin_rule", "ptype"), want: "IDX_CASBIN_RULE_PTYPE"},
		{name: "unique", got: ns.UniqueName("casbin_rule", "v0"), want: "UNI_CASBIN_RULE_V0"},
		{name: "checker", got: ns.CheckerName("t", "c"), want: "CHK_T_C"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}
