//go:build !linux

package host

import (
	"errors"
	"os"
	"testing"
)

func TestUnsupportedHostLock(t *testing.T) {
	s := newTestStore(t)
	if lock, err := s.Acquire(); lock != nil || !errors.Is(err, ErrUnsupportedHost) {
		t.Fatal(lock, err)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil || len(entries) != 0 {
		t.Fatal("platform guard wrote", entries, err)
	}
}
