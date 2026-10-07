//go:build linux

package host

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func (s *Store) Acquire() (io.Closer, error) {
	if s.uid() != 0 {
		return nil, errors.New("host lock requires root")
	}
	self, err := s.namespace("/proc/self/ns/mnt")
	if err != nil {
		return nil, err
	}
	init, err := s.namespace("/proc/1/ns/mnt")
	if err != nil {
		return nil, err
	}
	if self == "" || self != init {
		return nil, errors.New("split mount namespace")
	}
	dir, err := s.openRoot()
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := syscall.Openat(int(dir.Fd()), "supervisor.lock", syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), filepath.Join(s.root, "supervisor.lock"))
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Nlink != 1 {
		f.Close()
		return nil, errors.New("unsafe host lock inode")
	}
	if err := s.trustCheck(f.Name(), info); err != nil {
		f.Close()
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
