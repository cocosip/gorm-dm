package dm

import (
	"errors"

	dmdrv "gitee.com/chunanyong/dm"
)

// DmError error codes returned by the DaMeng server that this driver
// handles specially. See create.go for usage.
const (
	// errCodeNoIdentityColumn is returned by SET IDENTITY_INSERT when the
	// table has no identity column (-2717).
	errCodeNoIdentityColumn int32 = -2717
)

// AsDmError extracts a *dmdrv.DmError from err, unwrapping wrapped errors.
// It reports false when err is not a DaMeng error.
func AsDmError(err error) (*dmdrv.DmError, bool) {
	var dmErr *dmdrv.DmError
	if errors.As(err, &dmErr) {
		return dmErr, true
	}
	return nil, false
}

// isDmErrorCode reports whether err is a DaMeng error carrying the given
// error code.
func isDmErrorCode(err error, code int32) bool {
	dmErr, ok := AsDmError(err)
	return ok && dmErr.ErrCode == code
}
