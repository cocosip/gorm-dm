package dm

import (
	"errors"
	"fmt"
	"testing"

	dmdrv "gitee.com/chunanyong/dm"
)

func TestAsDmError(t *testing.T) {
	dmErr := &dmdrv.DmError{ErrCode: -2717, ErrText: "no identity column"}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"dm error", dmErr, true},
		{"wrapped dm error", fmt.Errorf("exec failed: %w", dmErr), true},
		{"wrapped plain error", fmt.Errorf("exec failed: %w", errors.New("boom")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := AsDmError(tt.err)
			if ok != tt.want {
				t.Fatalf("AsDmError(%v) ok = %v, want %v", tt.err, ok, tt.want)
			}
			if ok && got.ErrCode != -2717 {
				t.Fatalf("AsDmError() code = %d, want -2717", got.ErrCode)
			}
		})
	}
}

func TestIsDmErrorCode(t *testing.T) {
	dmErr := &dmdrv.DmError{ErrCode: -2717, ErrText: "no identity column"}
	if !isDmErrorCode(dmErr, errCodeNoIdentityColumn) {
		t.Fatal("isDmErrorCode(-2717 dm error, -2717) = false, want true")
	}
	// -7019 is any other DaMeng error code.
	if isDmErrorCode(fmt.Errorf("wrap: %w", dmErr), -7019) {
		t.Fatal("isDmErrorCode(-2717 dm error, -7019) = true, want false")
	}
	if isDmErrorCode(errors.New("boom"), errCodeNoIdentityColumn) {
		t.Fatal("isDmErrorCode(plain error, -2717) = true, want false")
	}
	if isDmErrorCode(nil, errCodeNoIdentityColumn) {
		t.Fatal("isDmErrorCode(nil, -2717) = true, want false")
	}
}
