//go:build !linux

package evidence

import (
	"fmt"
	"os"
)

// platformMountID is unsupported off Linux. There is no portable mount identity
// primitive, so production callers fail closed. Only in-package tests inject a
// private mount-id reader through the package seam (spec 6.1).
func platformMountID(*os.File) (uint64, error) {
	return 0, fmt.Errorf("%w: mount identity unsupported on this platform", ErrEvidenceTrust)
}
