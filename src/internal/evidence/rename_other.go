//go:build !linux

package evidence

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplaceAt emulates atomic no-replace with a descriptor-relative
// hardlink followed by removal of the temp name. After the hardlink the final
// already exists, so a concurrent creator loses the link race; the temp is
// removed to keep nlink=1.
func renameNoReplaceAt(dir *os.File, tmp, final string) error {
	dfd := int(dir.Fd())
	if err := unix.Linkat(dfd, tmp, dfd, final, 0); err != nil {
		return err
	}
	return unix.Unlinkat(dfd, tmp, 0)
}
