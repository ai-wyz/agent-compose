package guestabi

import (
	"errors"
	"fmt"
)

// CurrentGuestAbi is the guest ABI this daemon release speaks.
const CurrentGuestAbi = 1

// ErrUnsupportedGuestAbi reports a guest ABI manifest this daemon cannot speak.
var ErrUnsupportedGuestAbi = errors.New("unsupported guest abi")

// VerifyCompatible reports whether this daemon can speak the manifest's ABI. The
// error names both numbers so an operator can tell which side must change.
func (m Manifest) VerifyCompatible() error {
	if m.GuestAbi != CurrentGuestAbi {
		return fmt.Errorf("%w: manifest declares guest abi %d, daemon speaks %d", ErrUnsupportedGuestAbi, m.GuestAbi, CurrentGuestAbi)
	}
	return nil
}
