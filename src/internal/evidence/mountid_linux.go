//go:build linux

package evidence

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// mountIDSupport caches the one-time kernel capability probe. statx exists on
// old kernels but may return ENOSYS or omit STATX_MNT_ID; once support is known
// to be absent the caller fails closed instead of retrying the probe.
var mountIDSupport struct {
	once sync.Once
	err  error
}

// statxMountIDAvailable probes the running kernel once for STATX_MNT_ID support
// using the cheap, non-syncing statx form. Any unsupported result is retained.
func statxMountIDAvailable() error {
	mountIDSupport.once.Do(func() {
		var st unix.Statx_t
		err := unix.Statx(unix.AT_FDCWD, "/", unix.AT_STATX_DONT_SYNC, unix.STATX_MNT_ID, &st)
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.EOPNOTSUPP) {
			mountIDSupport.err = fmt.Errorf("%w: statx mount id unsupported", ErrEvidenceTrust)
			return
		}
		if err != nil {
			mountIDSupport.err = fmt.Errorf("%w: statx mount id unavailable: %v", ErrEvidenceTrust, err)
			return
		}
		if st.Mask&unix.STATX_MNT_ID == 0 {
			mountIDSupport.err = fmt.Errorf("%w: statx mount id unsupported", ErrEvidenceTrust)
		}
	})
	return mountIDSupport.err
}

// platformMountID returns the kernel mount identity of the opened descriptor f
// using statx(AT_EMPTY_PATH). Reading the identity from the pinned descriptor
// itself means a pathname lookup cannot race between the identity read and the
// pinned root. A kernel without reliable STATX_MNT_ID fails closed.
func platformMountID(f *os.File) (uint64, error) {
	if err := statxMountIDAvailable(); err != nil {
		return 0, err
	}
	var st unix.Statx_t
	if err := unix.Statx(int(f.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_STATX_DONT_SYNC, unix.STATX_MNT_ID, &st); err != nil {
		return 0, fmt.Errorf("%w: statx mount id: %v", ErrEvidenceTrust, err)
	}
	if st.Mask&unix.STATX_MNT_ID == 0 {
		return 0, fmt.Errorf("%w: statx mount id unsupported", ErrEvidenceTrust)
	}
	if st.Mnt_id == 0 {
		return 0, fmt.Errorf("%w: mount id unavailable", ErrEvidenceTrust)
	}
	return st.Mnt_id, nil
}
