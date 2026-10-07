//go:build linux

package evidence

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplaceAt atomically renames tmp to final within the pinned directory
// FD without ever replacing an existing final, leaving the final inode with
// nlink=1 and no lingering temp.
func renameNoReplaceAt(dir *os.File, tmp, final string) error {
	return unix.Renameat2(int(dir.Fd()), tmp, int(dir.Fd()), final, unix.RENAME_NOREPLACE)
}
