//go:build linux

package host

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func linuxTestStore(t *testing.T) *Store {
	t.Helper()
	s := newTestStore(t)
	s.uid = func() int { return 0 }
	s.namespace = func(string) (string, error) { return "mnt:[test]", nil }
	return s
}

func TestOneHostLock(t *testing.T) {
	s := linuxTestStore(t)
	lock, err := s.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, "supervisor.lock")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	other := linuxTestStore(t)
	other.root = s.root
	if duplicate, err := other.Acquire(); err == nil {
		duplicate.Close()
		t.Fatal("duplicate lock")
	}
	f := lock.(*os.File)
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFD, 0)
	if errno != 0 || flags&syscall.FD_CLOEXEC == 0 {
		t.Fatal("inheritable lock", errno)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err = other.Acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("lock inode replaced")
	}
}

func TestNamespaceSplitRejected(t *testing.T) {
	s := linuxTestStore(t)
	s.namespace = func(path string) (string, error) { return path, nil }
	if lock, err := s.Acquire(); err == nil {
		lock.Close()
		t.Fatal("namespace split accepted")
	}
	if _, err := os.Stat(filepath.Join(s.root, "supervisor.lock")); !os.IsNotExist(err) {
		t.Fatal("lock created before guard", err)
	}
}

func TestUnsafeLockPathRejected(t *testing.T) {
	t.Run("uid", func(t *testing.T) {
		s := linuxTestStore(t)
		s.uid = func() int { return 1000 }
		if lock, err := s.Acquire(); err == nil {
			lock.Close()
			t.Fatal("non-root")
		}
	})
	t.Run("directory", func(t *testing.T) {
		s := linuxTestStore(t)
		os.Chmod(s.root, 0755)
		if lock, err := s.Acquire(); err == nil {
			lock.Close()
			t.Fatal("unsafe directory")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		s := linuxTestStore(t)
		target := filepath.Join(s.root, "target")
		os.WriteFile(target, nil, 0600)
		os.Symlink(target, filepath.Join(s.root, "supervisor.lock"))
		if lock, err := s.Acquire(); err == nil {
			lock.Close()
			t.Fatal("lock symlink")
		}
	})
	t.Run("root-symlink", func(t *testing.T) {
		s := linuxTestStore(t)
		alias := filepath.Join(t.TempDir(), "alias")
		os.Symlink(s.root, alias)
		s.root = alias
		if lock, err := s.Acquire(); err == nil {
			lock.Close()
			t.Fatal("root symlink")
		}
	})
}

func TestLockOpenedInodeOwnership(t *testing.T) {
	for _, target := range []string{"root", "supervisor.lock"} {
		t.Run(target, func(t *testing.T) {
			s := linuxTestStore(t)
			seen := false
			s.trustCheck = func(path string, info os.FileInfo) error {
				if (target == "root" && path == s.root) || info.Name() == target {
					seen = true
					return trustedInode(path, withOwner(info, 1000))
				}
				return nil
			}
			if lock, err := s.Acquire(); err == nil {
				lock.Close()
				t.Fatal("untrusted lock inode accepted")
			}
			if !seen {
				t.Fatal("opened inode not checked")
			}
		})
	}
}

func TestLockRejectsUnsafeFileTypes(t *testing.T) {
	for _, kind := range []string{"directory", "fifo", "mode", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			s := linuxTestStore(t)
			path := filepath.Join(s.root, "supervisor.lock")
			var err error
			switch kind {
			case "directory":
				err = os.Mkdir(path, 0600)
			case "fifo":
				err = syscall.Mkfifo(path, 0600)
			case "mode":
				err = os.WriteFile(path, nil, 0644)
			case "hardlink":
				err = os.WriteFile(path, nil, 0600)
				if err == nil {
					err = os.Link(path, filepath.Join(s.root, "alias"))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if lock, err := s.Acquire(); err == nil {
				lock.Close()
				t.Fatal("unsafe lock file accepted")
			}
		})
	}
}
