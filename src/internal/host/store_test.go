package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"gha-runner-tui/internal/config"
)

func TestRequestSequenceReplay(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	r1, r2 := request(1, "drain"), request(2, "resume")
	if _, err := s.Submit(r1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(r2); err == nil {
		t.Fatal("pending overwritten")
	}
	commitControlForTest(t, s, r1, PolicyHold)
	if _, err := s.Submit(r2); err != nil {
		t.Fatal(err)
	}
	commitControlForTest(t, s, r2, PolicyRun)
	if _, err := s.Submit(r1); !errors.Is(err, ErrStaleRequest) {
		t.Fatal(err)
	}
	receipt, err := s.Submit(r2)
	if err != nil || receipt == nil || receipt.ID != r2.ID || receipt.Result != "APPLIED" {
		t.Fatal(receipt, err)
	}
	changed := r2
	changed.Action = "drain"
	if _, err := s.Submit(changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || got.Control.Policy != PolicyRun || got.Journal.HighWater != 2 || got.Journal.Applying != nil {
		t.Fatal(got, err)
	}
}

func TestControlDecodeRejectsInvalid(t *testing.T) {
	valid := `{"version":1,"id":{"host_id":"h1","seq":1},"action":"drain"}`
	for _, data := range []string{
		strings.Replace(valid, `"version":1`, `"version":1,"extra":0`, 1),
		strings.Replace(valid, `"version":1`, `"Version":1`, 1),
		strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(valid, `"seq":1`, `"seq":1,"seq":1`, 1),
		strings.Replace(valid, `"action":"drain"`, `"action":"drain","profile_id":"p"`, 1),
		strings.Replace(valid, `"drain"`, `"enable-profile"`, 1),
		strings.Replace(valid, `"drain"`, `"stop"`, 1),
		strings.Replace(valid, `"seq":1`, `"seq":0`, 1),
		strings.Replace(valid, `"host_id":"h1"`, `"host_id":""`, 1),
		valid + `{}`, valid + strings.Repeat(" ", 65536),
		`{"version":1,"id":{"host_id":"h1","seq":1},"action":"enable-profile","profile_id":"../p","binding":{"source":"/profiles/p.yaml","config_digest":"d"}}`,
		`{"version":1,"id":{"host_id":"h1","seq":1},"action":"enable-profile","profile_id":"p","binding":{"Source":"/profiles/p.yaml","config_digest":"d"}}`,
	} {
		if _, err := DecodeControlRequest(strings.NewReader(data)); err == nil {
			t.Fatalf("accepted %q", data[:min(len(data), 200)])
		}
	}
	a, err := DecodeControlRequest(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeControlRequest(strings.NewReader(`{"action":"drain", "id":{"seq":1,"host_id":"h1"},"version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	da, _ := RequestDigest(a)
	db, _ := RequestDigest(b)
	if da != db {
		t.Fatal("raw JSON used as identity")
	}
	for _, action := range []string{"enable-profile", "disable-profile"} {
		r := request(1, action)
		r.ProfileID = "p"
		r.Binding = &config.ParticipationBinding{Source: "/profiles/p.yaml", ConfigDigest: "digest"}
		data, _ := json.Marshal(r)
		if got, err := DecodeControlRequest(bytes.NewReader(data)); err != nil || !reflect.DeepEqual(got, r) {
			t.Fatal(got, err)
		}
		for _, profileID := range []string{"P", "p_name", "p.name", "p-"} {
			r.ProfileID = profileID
			data, _ := json.Marshal(r)
			if _, err := DecodeControlRequest(bytes.NewReader(data)); err == nil {
				t.Fatal("invalid managed profile name", profileID)
			}
		}
	}
}

func TestSubmitEncodedRequestLimit(t *testing.T) {
	base := request(1, "enable-profile")
	base.ProfileID = "p"
	base.Binding = &config.ParticipationBinding{Source: "/profiles/p.yaml", ConfigDigest: "d"}
	data, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	// The empty-digest envelope plus digest and encoder newline is exactly 64KiB.
	boundary := 65536 - len(data)
	for _, tc := range []struct {
		name, digest string
		valid        bool
	}{
		{"boundary", strings.Repeat("d", boundary), true},
		{"newline-overflow", strings.Repeat("d", boundary+1), false},
		{"plain-huge", strings.Repeat("d", 65536), false},
		{"escaped", strings.Repeat("<", 12000), false},
		{"control-escaped", strings.Repeat("\x01", 12000), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seed(t, s)
			r := base
			binding := *base.Binding
			binding.ConfigDigest = tc.digest
			r.Binding = &binding
			before, err := os.ReadFile(filepath.Join(s.root, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.syncFile = func(f *os.File) error { calls++; return f.Sync() }
			s.link = func(a, b string) error { calls++; return os.Link(a, b) }
			_, err = s.Submit(r)
			if tc.valid {
				if err != nil {
					t.Fatal(err)
				}
				published, err := os.ReadFile(filepath.Join(s.root, "request.json"))
				if err != nil || len(published) != 65536 || published[len(published)-1] != '\n' {
					t.Fatal("wrong boundary bytes", len(published), err)
				}
				got, err := s.TakeRequest()
				if err != nil || got == nil || !reflect.DeepEqual(*got, r) {
					t.Fatal("publication unreadable", err)
				}
				return
			}
			if !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("oversize publication accepted: %v", err)
			}
			if calls != 0 {
				t.Fatal("oversize performed publication I/O")
			}
			entries, err := os.ReadDir(s.root)
			if err != nil || len(entries) != 1 || entries[0].Name() != "state.json" {
				t.Fatal("oversize left files", entries, err)
			}
			after, err := os.ReadFile(filepath.Join(s.root, "state.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("oversize changed journal", err)
			}
			if _, err := RequestDigest(r); !errors.Is(err, ErrRequestConflict) {
				t.Fatal("oversize digest accepted", err)
			}
		})
	}
}

func TestJournalSurvivesRestart(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	r := request(1, "drain")
	commitControlForTest(t, s, r, PolicyHold)
	st, _ := s.Load()
	r2 := request(2, "resume")
	st.Journal.Applying = &r2
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := testStoreAt(s.root).Load()
	if err != nil || !reflect.DeepEqual(got, st) {
		t.Fatal(got, err)
	}
}

func TestSequenceCannotResetOrWrap(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	r := request(1, "drain")
	commitControlForTest(t, s, r, PolicyHold)
	st, _ := s.Load()
	reset := InitialState("h1")
	reset.Phase = "idle"
	if err := s.Save(reset); !errors.Is(err, ErrCorruptState) {
		t.Fatal("reset", err)
	}
	other := st
	other.HostID = "h2"
	if err := s.Save(other); !errors.Is(err, ErrHostMismatch) {
		t.Fatal("host reset", err)
	}
	r.ID.HostID = "h2"
	if _, err := CheckRequest(st, r); !errors.Is(err, ErrHostMismatch) {
		t.Fatal(err)
	}
	if _, err := CheckRequest(st, request(3, "resume")); !errors.Is(err, ErrOutOfOrder) {
		t.Fatal(err)
	}
	st.Journal.HighWater = math.MaxUint64
	st.Journal.Last = &Receipt{ID: RequestID{HostID: "h1", Seq: math.MaxUint64}, Digest: strings.Repeat("a", 64), Result: "APPLIED"}
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(request(1, "resume")); err == nil {
		t.Fatal("wrapped")
	}
	if _, err := s.Submit(request(0, "resume")); err == nil {
		t.Fatal("zero")
	}
	data, _ := os.ReadFile(filepath.Join(s.root, "state.json"))
	var raw map[string]json.RawMessage
	json.Unmarshal(data, &raw)
	delete(raw, "journal")
	data, _ = json.Marshal(raw)
	os.WriteFile(filepath.Join(s.root, "state.json"), data, 0600)
	if _, err := s.Load(); !errors.Is(err, ErrCorruptState) {
		t.Fatal("missing journal", err)
	}
}

func TestRequestPublicationAtomic(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	entered, release := make(chan struct{}, 2), make(chan struct{})
	s.link = func(a, b string) error { entered <- struct{}{}; <-release; return os.Link(a, b) }
	errCh := make(chan error, 2)
	for _, r := range []Request{request(1, "drain"), request(1, "resume")} {
		go func(r Request) { _, err := s.Submit(r); errCh <- err }(r)
	}
	<-entered
	<-entered
	if r, err := s.TakeRequest(); err != nil || r != nil {
		t.Fatal("temporary file visible", r, err)
	}
	close(release)
	e1, e2 := <-errCh, <-errCh
	if !((e1 == nil && errors.Is(e2, ErrRequestConflict)) || (e2 == nil && errors.Is(e1, ErrRequestConflict))) {
		t.Fatal(e1, e2)
	}
	r, err := s.TakeRequest()
	if err != nil || r == nil || r.ID.Seq != 1 {
		t.Fatal(r, err)
	}
	if _, err := s.Submit(*r); err != nil {
		t.Fatal("winner not idempotent", err)
	}
}

func TestReplayReceiptRequiresSync(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	r := request(1, "resume")
	st, _ := s.Load()
	st.Journal.Applying = &r
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	st.Control.Policy = PolicyRun
	st, _ = CompleteRequest(st, r, "APPLIED")
	entered, release := make(chan struct{}), make(chan struct{})
	s.syncDir = func(string) error { close(entered); <-release; return errors.New("dirsync uncertain") }
	errCh := make(chan error, 1)
	go func() { errCh <- s.Save(st) }()
	<-entered
	replay := testStoreAt(s.root)
	replay.syncFile = func(*os.File) error { return errors.New("sync failed") }
	if receipt, err := replay.Submit(r); err == nil || receipt != nil {
		t.Fatal("ack before durable sync", receipt, err)
	}
	close(release)
	if err := <-errCh; err == nil {
		t.Fatal("save acknowledged")
	}
	receipt, err := testStoreAt(s.root).Submit(r)
	if err != nil || receipt == nil {
		t.Fatal(receipt, err)
	}
}

func TestStoreLoadReadOnly(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	path := filepath.Join(s.root, "state.json")
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	calls := 0
	failure := errors.New("forbidden write")
	s.syncFile = func(*os.File) error { calls++; return failure }
	s.syncDir = func(string) error { calls++; return failure }
	s.rename = func(string, string) error { calls++; return failure }
	s.link = s.rename
	if _, err := s.Load(); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	now, _ := os.Stat(path)
	if calls != 0 || !bytes.Equal(before, after) || !info.ModTime().Equal(now.ModTime()) {
		t.Fatal("load wrote")
	}
	os.Remove(path)
	if _, err := s.Load(); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(s.root)
	if calls != 0 || len(entries) != 0 {
		t.Fatal("missing load initialized")
	}
}

func TestStoreFaultsPreserveIntent(t *testing.T) {
	for _, at := range []string{"sync", "rename", "dir"} {
		t.Run(at, func(t *testing.T) {
			s := newTestStore(t)
			seed(t, s)
			st, _ := s.Load()
			a := fixtureAdmission()
			st.Active = &a
			st.Phase = "running"
			if err := s.Save(st); err != nil {
				t.Fatal(err)
			}
			st.Control.Policy = PolicyRun
			fail := errors.New("injected")
			switch at {
			case "sync":
				s.syncFile = func(*os.File) error { return fail }
			case "rename":
				s.rename = func(string, string) error { return fail }
			case "dir":
				s.syncDir = func(string) error { return fail }
			}
			if err := s.Save(st); !errors.Is(err, fail) {
				t.Fatal(err)
			}
			got, err := s.Load()
			if err != nil || got.HostID != "h1" || got.Active == nil || got.Active.ID != "a" || (at != "dir" && got.Control.Policy != PolicyHold) {
				t.Fatal(got, err)
			}
		})
	}
	for _, at := range []string{"sync", "link", "dir"} {
		t.Run("publish-"+at, func(t *testing.T) {
			s := newTestStore(t)
			seed(t, s)
			fail := errors.New("injected")
			switch at {
			case "sync":
				s.syncFile = func(*os.File) error { return fail }
			case "link":
				s.link = func(string, string) error { return fail }
			case "dir":
				s.syncDir = func(string) error { return fail }
			}
			if ack, err := s.Submit(request(1, "drain")); err == nil || ack != nil {
				t.Fatal(ack, err)
			}
			pending, err := s.TakeRequest()
			if err != nil || (at != "dir" && pending != nil) {
				t.Fatal(pending, err)
			}
			st, _ := s.Load()
			if st.Journal.HighWater != 0 || st.Control.Policy != PolicyHold {
				t.Fatal("publish changed control")
			}
		})
	}
}

func TestControlMissingPolicyFailsClosed(t *testing.T) {
	for _, mutation := range []func(map[string]any){
		func(m map[string]any) { delete(m, "control") },
		func(m map[string]any) { m["control"] = map[string]any{} },
		func(m map[string]any) { m["control"] = map[string]any{"policy": "unknown"} },
		func(m map[string]any) { m["journal"] = map[string]any{} },
		func(m map[string]any) { m["control"] = nil },
	} {
		s := newTestStore(t)
		seed(t, s)
		mutateState(t, s, mutation)
		if _, err := s.Load(); !errors.Is(err, ErrCorruptState) {
			t.Fatal(err)
		}
	}
	s := newTestStore(t)
	seed(t, s)
	st, _ := s.Load()
	if st.Control.Policy != PolicyHold {
		t.Fatal(st)
	}
	record := ReleaseRecord{Path: "B", Physical: PhysicalProof{HostID: "h1", AdmissionID: "a", Digest: "physical"}, JobConclusion: "unknown"}
	st.LastRelease = &record
	summary := SummarizeRelease(record)
	st.LastReleased = &summary
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	st.LastReleased.PhysicalDigest = "tampered"
	if err := s.Save(st); !errors.Is(err, ErrCorruptState) {
		t.Fatal(err)
	}
}

func mutateState(t *testing.T, s *Store, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(s.root, "state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	data, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func fixtureAdmission() Admission {
	return Admission{ID: "a", HostID: "h1", ProfileID: "p", Phase: "running", RuntimeSnapshot: config.RuntimeSnapshot{Source: "/profiles/p.yaml", ConfigDigest: "digest", Credential: config.CredentialRef{ID: "cred"}}}
}

func TestAdmissionDrainPresenceRequired(t *testing.T) {
	for _, debt := range []bool{false, true} {
		for _, field := range []string{"drain", "requested", "source", "config_digest", "execution", "credential"} {
			t.Run(field+map[bool]string{false: "-active", true: "-debt"}[debt], func(t *testing.T) {
				s := newTestStore(t)
				seed(t, s)
				st, _ := s.Load()
				a := fixtureAdmission()
				if debt {
					st.Debts = []Debt{{Admission: a}}
				} else {
					st.Active = &a
					st.Phase = "running"
				}
				if err := s.Save(st); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Load(); err != nil {
					t.Fatal("explicit false", err)
				}
				mutateState(t, s, func(m map[string]any) {
					var adm map[string]any
					if debt {
						adm = m["debts"].([]any)[0].(map[string]any)["Admission"].(map[string]any)
					} else {
						adm = m["active"].(map[string]any)
					}
					if field == "requested" {
						delete(adm["drain"].(map[string]any), field)
					} else {
						delete(adm, field)
					}
				})
				if _, err := s.Load(); !errors.Is(err, ErrCorruptState) {
					t.Fatal(err)
				}
			})
		}
	}
	s := newTestStore(t)
	seed(t, s)
	st, _ := s.Load()
	a := fixtureAdmission()
	a.Drain.Requested = true
	st.Active = &a
	st.Phase = "running"
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	st.Active.Drain.Requested = false
	if err := s.Save(st); !errors.Is(err, ErrCorruptState) {
		t.Fatal("drain reset", err)
	}
	st.Active.Drain.Requested = true
	st.Phase = "idle"
	if err := s.Save(st); !errors.Is(err, ErrCorruptState) {
		t.Fatal("phase mismatch", err)
	}
}

func TestInitNoReplaceNeverResetsState(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	r := request(1, "drain")
	commitControlForTest(t, s, r, PolicyHold)
	if err := s.InitNoReplace(InitialState("h2")); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	st, _ := s.Load()
	if st.HostID != "h1" || st.Journal.HighWater != 1 {
		t.Fatal(st)
	}
	s = newTestStore(t)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, id := range []string{"h1", "h2"} {
		wg.Add(1)
		go func(id string) { defer wg.Done(); errs <- testStoreAt(s.root).InitNoReplace(InitialState(id)) }(id)
	}
	wg.Wait()
	e1, e2 := <-errs, <-errs
	if !((e1 == nil && errors.Is(e2, os.ErrExist)) || (e2 == nil && errors.Is(e1, os.ErrExist))) {
		t.Fatal(e1, e2)
	}
	st, err := s.Load()
	if err != nil || st.Control.Policy != PolicyHold || st.Journal.HighWater != 0 {
		t.Fatal(st, err)
	}
}

func TestCleanupRequiresReceiptAndMatchesFinal(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	r := request(1, "drain")
	if _, err := s.Submit(r); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupRequest(r); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("unacknowledged removed", err)
	}
	commitControlForTest(t, s, r, PolicyHold)
	if err := s.CleanupRequest(r); err != nil {
		t.Fatal("missing not idempotent", err)
	}
	data, _ := json.Marshal(request(1, "resume"))
	os.WriteFile(filepath.Join(s.root, "request.json"), data, 0600)
	if err := s.CleanupRequest(r); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("different final removed", err)
	}
	// A rejected complete stale request is cleaned without fabricating a receipt.
	r2 := request(2, "resume")
	st, _ := s.Load()
	st.Journal.Applying = &r2
	st, _ = CompleteRequest(st, r2, "APPLIED")
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupRequest(request(1, "resume")); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Load()
	if st.Journal.HighWater != 2 || st.Journal.Last.ID.Seq != 2 {
		t.Fatal(st)
	}
	data, _ = json.Marshal(r2)
	os.WriteFile(filepath.Join(s.root, "request.json"), data, 0600)
	s.syncDir = func(string) error { return errors.New("unlink sync failed") }
	if err := s.CleanupRequest(r2); err == nil {
		t.Fatal("cleanup sync acknowledged")
	}
}

func TestStoreSymlinksRejected(t *testing.T) {
	s := newTestStore(t)
	other := newTestStore(t)
	seed(t, other)
	if err := os.Symlink(filepath.Join(other.root, "state.json"), filepath.Join(s.root, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("state symlink read")
	}
	if err := s.Save(InitialState("h1")); err == nil {
		t.Fatal("state symlink overwritten")
	}
	link := filepath.Join(s.root, "alias")
	os.Symlink(other.root, link)
	if _, err := testStoreAt(link).Load(); err == nil {
		t.Fatal("root symlink read")
	}
	parent := newTestStore(t)
	alias := filepath.Join(parent.root, "alias")
	if err := os.Symlink(filepath.Dir(other.root), alias); err != nil {
		t.Fatal(err)
	}
	if _, err := testStoreAt(filepath.Join(alias, filepath.Base(other.root))).Load(); err == nil {
		t.Fatal("ancestor symlink read")
	}
}

func TestProductionStoreRejectsUntrustedIO(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	if _, err := s.Submit(request(1, "drain")); err != nil {
		t.Fatal(err)
	}
	// A production-policy instance must not trust a user's private temp tree.
	production := newStore(s.root)
	for name, call := range map[string]func() error{
		"load":    func() error { _, err := production.Load(); return err },
		"take":    func() error { _, err := production.TakeRequest(); return err },
		"save":    func() error { return production.Save(InitialState("h1")) },
		"init":    func() error { return production.InitNoReplace(InitialState("h1")) },
		"submit":  func() error { _, err := production.Submit(request(1, "drain")); return err },
		"cleanup": func() error { return production.CleanupRequest(request(1, "drain")) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("untrusted production I/O accepted")
			}
		})
	}
}

type inodeOwnerInfo struct {
	os.FileInfo
	stat syscall.Stat_t
}

func (i inodeOwnerInfo) Sys() any { return &i.stat }

func withOwner(info os.FileInfo, uid uint32) os.FileInfo {
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid = uid
	return inodeOwnerInfo{FileInfo: info, stat: stat}
}

func TestStoreOpenedInodeOwnership(t *testing.T) {
	for _, target := range []string{"root", "ancestor", "state.json", "request.json", "temp", "trusted"} {
		t.Run(target, func(t *testing.T) {
			s := newTestStore(t)
			seed(t, s)
			r := request(1, "drain")
			if _, err := s.Submit(r); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(s.root, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			seen := false
			s.trustCheck = func(path string, info os.FileInfo) error {
				// Only ownership is synthetic. File type/mode and opened-inode
				// metadata are real; no privileged chown or root resources.
				selected := (target == "root" && path == s.root) ||
					(target == "ancestor" && path == filepath.Dir(s.root)) ||
					(target == info.Name()) || (target == "temp" && strings.HasPrefix(info.Name(), ".host-"))
				if selected {
					seen = true
					return trustedInode(path, withOwner(info, 1000))
				}
				if path == s.root || !info.IsDir() {
					return trustedInode(path, withOwner(info, 0))
				}
				return nil // private temp ancestors only
			}
			var callErr error
			switch target {
			case "request.json":
				_, callErr = s.TakeRequest()
			case "temp":
				st, err := s.Load()
				if err != nil {
					t.Fatal(err)
				}
				callErr = s.Save(st)
			default:
				_, callErr = s.Load()
			}
			if target == "trusted" {
				if callErr != nil {
					t.Fatal(callErr)
				}
			} else if callErr == nil || !seen {
				t.Fatal("untrusted opened inode accepted", callErr, seen)
			}
			after, err := os.ReadFile(filepath.Join(s.root, "state.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("ownership rejection changed state", err)
			}
		})
	}
}

func TestStoreRejectsWritableAncestor(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	parent := filepath.Dir(s.root)
	if err := os.Chmod(parent, 0777); err != nil {
		t.Fatal(err)
	}
	s.trustCheck = func(path string, info os.FileInfo) error {
		if path == parent {
			return trustedInode(path, withOwner(info, 0))
		}
		return nil
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("writable ancestor accepted")
	}
}

func TestStoreRejectsUnsafeFileTypes(t *testing.T) {
	for _, name := range []string{"state.json", "request.json"} {
		for _, kind := range []string{"directory", "fifo", "mode"} {
			t.Run(name+"-"+kind, func(t *testing.T) {
				s := newTestStore(t)
				path := filepath.Join(s.root, name)
				var err error
				switch kind {
				case "directory":
					err = os.Mkdir(path, 0600)
				case "fifo":
					err = syscall.Mkfifo(path, 0600)
				case "mode":
					err = os.WriteFile(path, []byte("{}"), 0644)
				}
				if err != nil {
					t.Fatal(err)
				}
				if f, err := s.openFile(name); err == nil {
					f.Close()
					t.Fatal("unsafe file accepted")
				}
			})
		}
	}
}

func TestReplayDirectorySyncAndCleanupRetry(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	r := request(1, "drain")
	commitControlForTest(t, s, r, PolicyHold)
	s.syncDir = func(string) error { return errors.New("dir sync failed") }
	if receipt, err := s.Submit(r); err == nil || receipt != nil {
		t.Fatal("receipt despite directory sync failure", receipt, err)
	}
	if err := s.CleanupRequest(r); err == nil {
		t.Fatal("missing file concealed uncertain cleanup")
	}
	s.syncDir = s.syncDirectory
	if err := s.CleanupRequest(r); err != nil {
		t.Fatal(err)
	}
}

func TestInitFaultBoundaries(t *testing.T) {
	for _, at := range []string{"sync", "link", "dir"} {
		t.Run(at, func(t *testing.T) {
			s := newTestStore(t)
			fail := errors.New("injected init failure")
			switch at {
			case "sync":
				s.syncFile = func(*os.File) error { return fail }
			case "link":
				s.link = func(string, string) error { return fail }
			case "dir":
				s.syncDir = func(string) error { return fail }
			}
			if err := s.InitNoReplace(InitialState("h1")); !errors.Is(err, fail) {
				t.Fatal(err)
			}
			st, err := s.Load()
			if at == "dir" {
				if err != nil || st.HostID != "h1" || st.Control.Policy != PolicyHold {
					t.Fatal("uncertain publication not retained", st, err)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("incomplete init visible", st, err)
			}
			entries, err := os.ReadDir(s.root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "state.json" {
					t.Fatal("temporary file leaked", entry.Name())
				}
			}
		})
	}
}

func TestSnapshotNestedPresenceRequired(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	st, _ := s.Load()
	a := fixtureAdmission()
	st.Active = &a
	st.Phase = "running"
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	mutateState(t, s, func(m map[string]any) { delete(m["active"].(map[string]any)["execution"].(map[string]any), "outer") })
	if _, err := s.Load(); !errors.Is(err, ErrCorruptState) {
		t.Fatal("missing frozen execution accepted", err)
	}
}

func TestApplyingCannotBeRewritten(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	st, _ := s.Load()
	r := request(1, "drain")
	st.Journal.Applying = &r
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	r = request(1, "resume")
	if err := s.Save(st); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("applying body changed", err)
	}
}

func TestReceiptMustCompletePersistedApplying(t *testing.T) {
	for _, change := range []string{"action", "binding", "id", "skip", "valid"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			seed(t, s)
			st, err := s.Load()
			if err != nil {
				t.Fatal(err)
			}
			r := request(1, "drain")
			if change == "binding" {
				r.Action, r.ProfileID = "enable-profile", "p"
				r.Binding = &config.ParticipationBinding{Source: "/profiles/p.yaml", ConfigDigest: "old"}
			}
			st.Journal.Applying = &r
			if err := s.Save(st); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(s.root, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "action":
				r.Action = "resume"
			case "binding":
				r.Binding.ConfigDigest = "changed"
			case "skip":
				r.ID.Seq = 2
			}
			if change == "skip" {
				st.Journal.HighWater = 1
			}
			st.Control.Policy = PolicyRun
			st, err = CompleteRequest(st, r, "APPLIED")
			if err != nil {
				t.Fatal(err)
			}
			if change == "id" {
				st.Journal.Last.ID.HostID = "other"
			}
			err = s.Save(st)
			if change == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.Load()
				if err != nil || got.Journal.HighWater != 1 || got.Journal.Applying != nil {
					t.Fatal(got, err)
				}
				return
			}
			if !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("changed completion accepted: %v", err)
			}
			after, err := os.ReadFile(filepath.Join(s.root, "state.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected completion changed disk", err)
			}
		})
	}
}

func TestDebtProofSurvivesLaterRelease(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	st, _ := s.Load()
	a := fixtureAdmission()
	old := ReleaseRecord{Path: "B", Physical: PhysicalProof{HostID: "h1", AdmissionID: "a", Digest: "a-proof", EvidenceIDs: []string{"a-evidence"}}, JobConclusion: "unknown"}
	st.Debts = []Debt{{Admission: a, Release: old, Reason: "remote stale", Scope: "repo"}}
	later := ReleaseRecord{Path: "A", Physical: PhysicalProof{HostID: "h1", AdmissionID: "c", Digest: "c-proof"}, JobConclusion: "success"}
	st.LastRelease = &later
	summary := SummarizeRelease(later)
	st.LastReleased = &summary
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := testStoreAt(s.root).Load()
	if err != nil || !reflect.DeepEqual(got.Debts[0].Release, old) || got.Debts[0].Admission.Credential.ID != "cred" {
		t.Fatal(got, err)
	}
}

func TestReleaseSummaryRoundTripsTimestamp(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	st, _ := s.Load()
	record := ReleaseRecord{Path: "B", Physical: PhysicalProof{AdmissionID: "a", Digest: "proof", ProvenAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.FixedZone("fixture", 19800))}}
	st.LastRelease = &record
	summary := SummarizeRelease(record)
	st.LastReleased = &summary
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil || !got.LastReleased.ReleasedAt.Equal(record.Physical.ProvenAt) {
		t.Fatal("equivalent timestamp rejected", got, err)
	}
}

func TestMalformedPendingBlocksAndRetainsFile(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	path := filepath.Join(s.root, "request.json")
	data := []byte(`{"version":1,"action":"drain"`)
	os.WriteFile(path, data, 0600)
	if _, err := s.TakeRequest(); err == nil {
		t.Fatal("malformed pending accepted")
	}
	if _, err := s.Submit(request(1, "drain")); err == nil {
		t.Fatal("malformed pending replaced")
	}
	if err := s.CleanupRequest(request(1, "drain")); err == nil {
		t.Fatal("malformed pending removed")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, data) {
		t.Fatal("malformed file changed")
	}
}

func TestRequestSameIDConflict(t *testing.T) {
	s := newTestStore(t)
	seed(t, s)
	r := request(1, "drain")
	other := request(1, "resume")
	if _, err := s.Submit(r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(other); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("pending", err)
	}
	st, _ := s.Load()
	st.Journal.Applying = &r
	if err := s.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(other); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("applying", err)
	}
	if disposition, err := CheckRequest(st, r); err != nil || disposition != RequestRecover {
		t.Fatal(disposition, err)
	}
	commitControlForTest(t, s, r, PolicyHold)
	if _, err := s.Submit(other); !errors.Is(err, ErrRequestConflict) {
		t.Fatal("last", err)
	}
}

func TestStoreRejectsCorruptSchema(t *testing.T) {
	for _, data := range []string{`{`, `{"schema_version":2}`, `{"schema_version":1,"schema_version":1}`, `{"Schema_version":1}`} {
		t.Run(data, func(t *testing.T) {
			s := newTestStore(t)
			if err := os.WriteFile(filepath.Join(s.root, "state.json"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(); !errors.Is(err, ErrCorruptState) {
				t.Fatal(err)
			}
		})
	}
}
