package evidence

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	evidenceDirMode  = 0700
	evidenceFileMode = 0600
	dirOpenFlags     = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
)

// mountIDFn reads the kernel mount identity of an opened descriptor. Production
// Linux supplies platformMountID (statx STATX_MNT_ID); it is a seam only so
// in-package tests on platforms without statx can inject a concrete identity.
type mountIDFn func(*os.File) (uint64, error)

// pinnedRoot is a validated, descriptor-pinned scheduler root. Every evidence
// path is resolved component-by-component with openat/mkdirat relative to this
// FD, so no pathname syscall can follow an ancestor symlink after validation
// and the root inode/mount identity cannot be swapped underneath the caller.
type pinnedRoot struct {
	path      string
	fd        *os.File
	dev       uint64
	ino       uint64
	mountID   uint64
	trust     func(string, os.FileInfo) error
	namespace func() error
}

// openPinnedRoot opens the canonical root one ancestor at a time, each relative
// to the previously pinned parent with O_NOFOLLOW|O_DIRECTORY, and validates
// the opened inode (not a pathname) for directory type and trust. The initial
// "/" descriptor is trust-checked too, the final scheduler root must be exactly
// 0700, and the returned FD stays pinned until Close; the recorded dev/inode and
// kernel mount identity are the retained root identity.
func openPinnedRoot(rootPath string, trust func(string, os.FileInfo) error, namespace func() error, mountID mountIDFn) (*pinnedRoot, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return nil, fmt.Errorf("%w: noncanonical root", ErrEvidenceTrust)
	}
	if namespace != nil {
		if err := namespace(); err != nil {
			return nil, err
		}
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	root := os.NewFile(uintptr(fd), "/")
	fail := func(err error) (*pinnedRoot, error) {
		root.Close()
		return nil, err
	}
	// The initial ancestor must itself be a trust-checked directory: the loop
	// below only validates opened components below "/", so without this the
	// root of the walk would be unverified (spec 6.1).
	initInfo, err := root.Stat()
	if err != nil {
		return fail(err)
	}
	if !initInfo.IsDir() {
		return fail(fmt.Errorf("%w: root not a directory", ErrEvidenceTrust))
	}
	if trust != nil {
		if err := trust("/", initInfo); err != nil {
			return fail(err)
		}
	}
	var segments []string
	for _, part := range strings.Split(rootPath, "/") {
		if part == "" || part == "." {
			continue
		}
		segments = append(segments, part)
	}
	if len(segments) == 0 {
		return fail(fmt.Errorf("%w: root has no components", ErrEvidenceTrust))
	}
	current := "/"
	for i, part := range segments {
		last := i == len(segments)-1
		current = filepath.Join(current, part)
		nfd, err := unix.Openat(int(root.Fd()), part, dirOpenFlags, 0)
		if err != nil {
			return fail(err)
		}
		next := os.NewFile(uintptr(nfd), current)
		info, err := next.Stat()
		if err == nil && !info.IsDir() {
			err = fmt.Errorf("%w: nondirectory ancestor %s", ErrEvidenceTrust, current)
		}
		if err == nil && last && info.Mode().Perm() != evidenceDirMode {
			err = fmt.Errorf("%w: scheduler root mode %s", ErrEvidenceTrust, current)
		}
		if err == nil && trust != nil {
			err = trust(current, info)
		}
		if err != nil {
			next.Close()
			return fail(err)
		}
		root.Close()
		root = next
	}
	info, err := root.Stat()
	if err != nil {
		return fail(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fail(fmt.Errorf("%w: root stat", ErrEvidenceTrust))
	}
	if mountID == nil {
		return fail(fmt.Errorf("%w: mount identity reader missing", ErrEvidenceTrust))
	}
	mount, err := mountID(root)
	if err != nil {
		return fail(err)
	}
	return &pinnedRoot{
		path:      rootPath,
		fd:        root,
		dev:       uint64(stat.Dev),
		ino:       uint64(stat.Ino),
		mountID:   mount,
		trust:     trust,
		namespace: namespace,
	}, nil
}

// Close releases the pinned root FD.
func (r *pinnedRoot) Close() error {
	if r == nil || r.fd == nil {
		return nil
	}
	return r.fd.Close()
}

// splitRel normalizes a relative evidence path to clean components, rejecting
// absolute paths and any ".." escape.
func splitRel(rel string) ([]string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return nil, fmt.Errorf("%w: bad relative path", ErrEvidenceTrust)
	}
	clean := filepath.ToSlash(filepath.Clean(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("%w: bad relative path", ErrEvidenceTrust)
	}
	var parts []string
	for _, p := range strings.Split(clean, "/") {
		if p == "" || p == "." {
			continue
		}
		parts = append(parts, p)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: bad relative path", ErrEvidenceTrust)
	}
	return parts, nil
}

// openDir opens a directory relative to the pinned root, validating each
// component is a 0700 evidence directory owned by the trust seam.
func (r *pinnedRoot) openDir(rel string) (*os.File, error) {
	parts, err := splitRel(rel)
	if err != nil {
		return nil, err
	}
	return r.walkDir(parts, false, nil)
}

// walkDir is the shared descriptor-relative directory walk. When create is set,
// missing components are made 0700 with mkdirat. When sync is non-nil the
// parent descriptor is fsynced after each component is ensured, including
// already-existing components, so a retry repairs a prior failed parent sync
// before any publication acknowledgement. The returned FD is owned by the
// caller; all intermediate FDs are closed.
func (r *pinnedRoot) walkDir(parts []string, create bool, sync func(*os.File) error) (*os.File, error) {
	var opened []*os.File
	fail := func(err error) (*os.File, error) {
		for _, f := range opened {
			f.Close()
		}
		return nil, err
	}
	dir := r.fd
	current := r.path
	for _, part := range parts {
		fd, err := unix.Openat(int(dir.Fd()), part, dirOpenFlags, 0)
		if err != nil {
			if !create || !errors.Is(err, unix.ENOENT) {
				return fail(err)
			}
			if err := unix.Mkdirat(int(dir.Fd()), part, evidenceDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
				return fail(err)
			}
			fd, err = unix.Openat(int(dir.Fd()), part, dirOpenFlags, 0)
			if err != nil {
				return fail(err)
			}
		}
		f := os.NewFile(uintptr(fd), part)
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return fail(err)
		}
		if !info.IsDir() || info.Mode().Perm() != evidenceDirMode {
			f.Close()
			return fail(fmt.Errorf("%w: unsafe evidence dir %s", ErrEvidenceTrust, part))
		}
		current = filepath.Join(current, part)
		if r.trust != nil {
			if err := r.trust(current, info); err != nil {
				f.Close()
				return fail(err)
			}
		}
		if sync != nil {
			if err := sync(dir); err != nil {
				f.Close()
				return fail(err)
			}
		}
		opened = append(opened, f)
		dir = f
	}
	last := opened[len(opened)-1]
	for _, f := range opened[:len(opened)-1] {
		f.Close()
	}
	return last, nil
}
