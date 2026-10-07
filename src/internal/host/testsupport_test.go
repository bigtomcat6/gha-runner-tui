package host

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	return testStoreAt(root)
}

// Trust only the test process's private tree, never production constructors.
func testStoreAt(root string) *Store {
	s := newStore(root)
	s.trustCheck = func(_ string, _ os.FileInfo) error { return nil }
	return s
}

func seed(t *testing.T, s *Store) {
	t.Helper()
	st := InitialState("h1")
	st.Phase = "idle"
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
}

func request(seq uint64, action string) Request {
	return Request{Version: 1, ID: RequestID{HostID: "h1", Seq: seq}, Action: action}
}

func commitControlForTest(t *testing.T, s *Store, req Request, policy AdmissionPolicy) {
	t.Helper()
	st, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	st.Journal.Applying = &req
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	st.Control.Policy = policy
	st, err = CompleteRequest(st, req, "APPLIED")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupRequest(req); err != nil {
		t.Fatal(err)
	}
}
