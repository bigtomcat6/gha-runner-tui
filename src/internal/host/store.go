package host

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"

	"gha-runner-tui/internal/config"
)

type Store struct {
	root       string
	syncFile   func(*os.File) error
	rename     func(string, string) error
	link       func(string, string) error
	syncDir    func(string) error
	uid        func() int
	namespace  func(string) (string, error)
	trustCheck func(string, os.FileInfo) error
}

func NewStore() *Store { return newStore("/var/lib/gha-runner-tui/host-scheduler") }
func newStore(root string) *Store {
	s := &Store{root: root, syncFile: func(f *os.File) error { return f.Sync() }, rename: os.Rename, link: os.Link, uid: os.Geteuid, namespace: os.Readlink, trustCheck: trustedInode}
	s.syncDir = s.syncDirectory
	return s
}

func trustedInode(_ string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || (info.IsDir() && info.Mode().Perm()&0022 != 0) {
		return errors.New("untrusted host state inode")
	}
	return nil
}

func (s *Store) syncDirectory(_ string) error {
	f, err := s.openRoot()
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Match config's managed profile names without introducing a second name policy.
var managedName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

const maxRequestBytes = 64 * 1024

func validateRequest(req Request) error {
	if req.Version != 1 || len(req.ID.HostID) > maxRequestBytes || len(req.ProfileID) > maxRequestBytes || strings.TrimSpace(req.ID.HostID) == "" || req.ID.Seq == 0 {
		return ErrRequestConflict
	}
	switch req.Action {
	case "drain", "resume":
		if req.ProfileID != "" || req.Binding != nil {
			return ErrRequestConflict
		}
	case "enable-profile", "disable-profile":
		if !managedName.MatchString(req.ProfileID) || req.Binding == nil || !validBinding(*req.Binding) {
			return ErrRequestConflict
		}
	default:
		return ErrRequestConflict
	}
	return nil
}

func validBinding(b config.ParticipationBinding) bool {
	return len(b.Source) <= maxRequestBytes && len(b.ConfigDigest) <= maxRequestBytes && filepath.IsAbs(b.Source) && filepath.Clean(b.Source) == b.Source && !strings.ContainsAny(b.Source, "\x00\r\n") && (filepath.Ext(b.Source) == ".yaml" || filepath.Ext(b.Source) == ".yml") && strings.TrimSpace(b.ConfigDigest) != ""
}

func DecodeControlRequest(r io.Reader) (Request, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxRequestBytes+1))
	if err != nil {
		return Request{}, err
	}
	if len(data) > maxRequestBytes {
		return Request{}, ErrRequestConflict
	}
	var req Request
	if err := strictJSON(data, &req); err != nil {
		return Request{}, fmt.Errorf("%w: %v", ErrRequestConflict, err)
	}
	if err := validateRequest(req); err != nil {
		return Request{}, err
	}
	return req, nil
}

// Walk tokens before typed decoding: encoding/json otherwise accepts duplicates
// and case-insensitive aliases. Anonymous snapshot fields retain their own tags.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "-" {
			continue
		}
		if f.Anonymous && tag == "" {
			for name, typ := range jsonFields(f.Type) {
				fields[name] = typ
			}
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		fields[tag] = f.Type
	}
	return fields
}

func walkJSON(d *json.Decoder, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{':
			if t.Kind() != reflect.Struct {
				return errors.New("unexpected object")
			}
			fields, seen := jsonFields(t), map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("invalid key")
				}
				typ, ok := fields[name]
				if !ok || seen[name] {
					return fmt.Errorf("unknown or duplicate field %q", name)
				}
				seen[name] = true
				if err := walkJSON(d, typ); err != nil {
					return err
				}
			}
		case '[':
			if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
				return errors.New("unexpected array")
			}
			for d.More() {
				if err := walkJSON(d, t.Elem()); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected delimiter")
		}
		_, err = d.Token()
		return err
	}
	return nil
}

func strictJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := walkJSON(d, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}

func RequestDigest(req Request) (string, error) {
	data, err := encodeRequest(req)
	if err != nil {
		return "", err
	}
	return requestDigest(data), nil
}

// Canonical typed JSON is also the publication body. Reserve the encoder's
// newline using subtraction, so neither escaping nor integer overflow can
// make a published request larger than the public reader's limit.
func encodeRequest(req Request) ([]byte, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(data) > maxRequestBytes-1 {
		return nil, ErrRequestConflict
	}
	return data, nil
}

func requestDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func CheckRequest(st HostState, req Request) (RequestDisposition, error) {
	digest, err := RequestDigest(req)
	if err != nil {
		return "", err
	}
	return checkRequest(st, req, digest)
}

func checkRequest(st HostState, req Request, digest string) (RequestDisposition, error) {
	if req.ID.HostID != st.HostID {
		return "", ErrHostMismatch
	}
	h := st.Journal.HighWater
	if req.ID.Seq < h {
		return "", ErrStaleRequest
	}
	if req.ID.Seq == h {
		last := st.Journal.Last
		if last == nil || last.ID.HostID != st.HostID || last.ID.Seq != h {
			return "", ErrCorruptState
		}
		if last.Digest != digest {
			return "", ErrRequestConflict
		}
		return RequestReplay, nil
	}
	if h == ^uint64(0) {
		return "", ErrSequenceExhausted
	}
	if req.ID.Seq != h+1 {
		return "", ErrOutOfOrder
	}
	if applying := st.Journal.Applying; applying != nil {
		saved, err := RequestDigest(*applying)
		if err != nil {
			return "", ErrCorruptState
		}
		if applying.ID != req.ID || saved != digest {
			return "", ErrRequestConflict
		}
		return RequestRecover, nil
	}
	return RequestAccept, nil
}

func CompleteRequest(st HostState, req Request, result string) (HostState, error) {
	disposition, err := CheckRequest(st, req)
	if err != nil {
		return st, err
	}
	if disposition != RequestRecover {
		return st, ErrRequestConflict
	}
	digest, err := RequestDigest(req)
	if err != nil {
		return st, err
	}
	st.Journal.HighWater = req.ID.Seq
	st.Journal.Last = &Receipt{ID: req.ID, Digest: digest, Result: result}
	st.Journal.Applying = nil
	return st, nil
}

func validateState(st HostState) error {
	if st.Schema != 1 || strings.TrimSpace(st.HostID) == "" || (st.Control.Policy != PolicyHold && st.Control.Policy != PolicyRun) {
		return ErrCorruptState
	}
	switch st.Phase {
	case "reconciling", "blocked":
	case "idle", "polling":
		if st.Active != nil {
			return ErrCorruptState
		}
	case "reserved", "starting", "registering", "waiting-for-job", "running", "draining", "cleaning":
		if st.Active == nil || st.Active.Phase != st.Phase {
			return ErrCorruptState
		}
	default:
		return ErrCorruptState
	}
	j := st.Journal
	if j.HighWater == 0 {
		if j.Last != nil {
			return ErrCorruptState
		}
	} else {
		if j.Last == nil || j.Last.ID.HostID != st.HostID || j.Last.ID.Seq != j.HighWater || len(j.Last.Digest) != 64 || j.Last.Result == "" {
			return ErrCorruptState
		}
		if _, err := hex.DecodeString(j.Last.Digest); err != nil {
			return ErrCorruptState
		}
	}
	if j.Applying != nil {
		if j.HighWater == ^uint64(0) || j.Applying.ID.HostID != st.HostID || j.Applying.ID.Seq != j.HighWater+1 || validateRequest(*j.Applying) != nil {
			return ErrCorruptState
		}
	}
	if (st.LastRelease == nil) != (st.LastReleased == nil) {
		return ErrCorruptState
	}
	if st.LastRelease != nil {
		want, got := SummarizeRelease(*st.LastRelease), *st.LastReleased
		if want.AdmissionID != got.AdmissionID || want.Path != got.Path || want.PhysicalDigest != got.PhysicalDigest || want.JobConclusion != got.JobConclusion || !want.ReleasedAt.Equal(got.ReleasedAt) {
			return ErrCorruptState
		}
	}
	if st.Active != nil && validateAdmission(*st.Active, st.HostID) != nil {
		return ErrCorruptState
	}
	for _, d := range st.Debts {
		if validateAdmission(d.Admission, st.HostID) != nil {
			return ErrCorruptState
		}
	}
	return nil
}

func validateAdmission(a Admission, hostID string) error {
	if a.ID == "" || a.HostID != hostID || !managedName.MatchString(a.ProfileID) || !validBinding(config.ParticipationBinding{Source: a.Source, ConfigDigest: a.ConfigDigest}) || a.Credential.ID == "" {
		return ErrCorruptState
	}
	switch a.Phase {
	case "reserved", "starting", "registering", "waiting-for-job", "running", "draining", "cleaning", "blocked", "reconciling":
		return nil
	}
	return ErrCorruptState
}

func object(data json.RawMessage) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return nil, ErrCorruptState
	}
	return m, nil
}

func present(m map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		v, ok := m[name]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return false
		}
	}
	return true
}

func admissionPresence(data json.RawMessage) error {
	m, err := object(data)
	if err != nil {
		return err
	}
	for name, typ := range jsonFields(reflect.TypeOf(config.RuntimeSnapshot{})) {
		if _, ok := m[name]; !ok {
			return ErrCorruptState
		}
		if typ.Kind() != reflect.Slice && !present(m, name) {
			return ErrCorruptState
		}
		if err := snapshotPresence(m[name], typ); err != nil {
			return err
		}
	}
	if !present(m, "drain") {
		return ErrCorruptState
	}
	drain, err := object(m["drain"])
	if err != nil || !present(drain, "requested") {
		return ErrCorruptState
	}
	return nil
}

func snapshotPresence(data json.RawMessage, t reflect.Type) error {
	switch t.Kind() {
	case reflect.Struct:
		m, err := object(data)
		if err != nil {
			return err
		}
		for name, typ := range jsonFields(t) {
			if _, ok := m[name]; !ok {
				return ErrCorruptState
			}
			if typ.Kind() != reflect.Slice && !present(m, name) {
				return ErrCorruptState
			}
			if err := snapshotPresence(m[name], typ); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return ErrCorruptState
		}
		for _, value := range values {
			if err := snapshotPresence(value, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeState(data []byte) (HostState, error) {
	var st HostState
	if err := strictJSON(data, &st); err != nil {
		return st, fmt.Errorf("%w: %v", ErrCorruptState, err)
	}
	m, err := object(data)
	if err != nil || !present(m, "schema_version", "host_id", "control", "journal") {
		return st, ErrCorruptState
	}
	c, err := object(m["control"])
	if err != nil || !present(c, "policy") {
		return st, ErrCorruptState
	}
	j, err := object(m["journal"])
	if err != nil || !present(j, "high_water") {
		return st, ErrCorruptState
	}
	if st.Active != nil {
		if err := admissionPresence(m["active"]); err != nil {
			return st, err
		}
	}
	if len(st.Debts) > 0 {
		var debts []json.RawMessage
		if err := json.Unmarshal(m["debts"], &debts); err != nil {
			return st, ErrCorruptState
		}
		for _, data := range debts {
			d, err := object(data)
			if err != nil {
				return st, err
			}
			if err := admissionPresence(d["Admission"]); err != nil {
				return st, err
			}
		}
	}
	return st, validateState(st)
}

func (s *Store) safeRoot() error {
	f, err := s.openRoot()
	if err != nil {
		return err
	}
	return f.Close()
}

// Every ancestor is root-owned and not writable by other users. Validate the
// opened directories too; path metadata alone is not an inode trust decision.
func (s *Store) openRoot() (*os.File, error) {
	if !filepath.IsAbs(s.root) || filepath.Clean(s.root) != s.root {
		return nil, errors.New("noncanonical host state directory")
	}
	var root *os.File
	fail := func(err error) (*os.File, error) {
		if root != nil {
			root.Close()
		}
		return nil, err
	}
	for current := s.root; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fail(err)
		}
		if !info.IsDir() {
			return fail(errors.New("symlink or nondirectory host state path"))
		}
		fd, err := syscall.Open(current, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return fail(err)
		}
		f := os.NewFile(uintptr(fd), current)
		opened, err := f.Stat()
		if err == nil && (!opened.IsDir() || !os.SameFile(info, opened) || (current == s.root && opened.Mode().Perm() != 0700)) {
			err = errors.New("unsafe host state directory")
		}
		if err == nil {
			err = s.trustCheck(current, opened)
		}
		if err != nil {
			f.Close()
			return fail(err)
		}
		if current == s.root {
			root = f
		} else {
			f.Close()
		}
		if current == "/" {
			break
		}
	}
	return root, nil
}

func (s *Store) openFile(name string) (*os.File, error) {
	if err := s.safeRoot(); err != nil {
		return nil, err
	}
	path := filepath.Join(s.root, name)
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, errors.New("unsafe host state file")
	}
	if err := s.trustCheck(path, info); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (s *Store) Load() (HostState, error) {
	f, err := s.openFile("state.json")
	if err != nil {
		return HostState{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return HostState{}, err
	}
	return decodeState(data)
}

func drainRegression(old, next HostState) bool {
	admissions := map[string]Admission{}
	if old.Active != nil {
		admissions[old.Active.ID] = *old.Active
	}
	for _, d := range old.Debts {
		admissions[d.Admission.ID] = d.Admission
	}
	check := func(a Admission) bool {
		prev, ok := admissions[a.ID]
		return ok && prev.Drain.Requested && !a.Drain.Requested
	}
	if next.Active != nil && check(*next.Active) {
		return true
	}
	for _, d := range next.Debts {
		if check(d.Admission) {
			return true
		}
	}
	return false
}

// Save is the sole supervisor writer's atomic replacement. It never creates
// directories; missing production state is initialized only by approved activation.
func (s *Store) Save(st HostState) error {
	old, err := s.Load()
	if err == nil {
		if old.HostID != st.HostID {
			return ErrHostMismatch
		}
		if st.Journal.HighWater < old.Journal.HighWater || drainRegression(old, st) {
			return ErrCorruptState
		}
		if old.Journal.HighWater == st.Journal.HighWater && !reflect.DeepEqual(old.Journal.Last, st.Journal.Last) {
			return ErrCorruptState
		}
		if old.Journal.HighWater == st.Journal.HighWater && old.Journal.Applying != nil && !reflect.DeepEqual(old.Journal.Applying, st.Journal.Applying) {
			return ErrRequestConflict
		}
		if old.Journal.Applying != nil && st.Journal.HighWater > old.Journal.HighWater {
			digest, err := RequestDigest(*old.Journal.Applying)
			last := st.Journal.Last
			if err != nil || last == nil || st.Journal.HighWater != old.Journal.Applying.ID.Seq || last.ID != old.Journal.Applying.ID || last.Digest != digest {
				return ErrRequestConflict
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := validateState(st); err != nil {
		return err
	}
	return s.publish(st, "state.json", false)
}

// Caller must hold the fixed lock and an approved, marker-bound activation.
// No ordinary startup/read/control path calls this method.
func (s *Store) InitNoReplace(st HostState) error {
	if err := validateState(st); err != nil {
		return err
	}
	if st.Phase != "reconciling" || st.Control.Policy != PolicyHold || st.Journal.HighWater != 0 || st.Journal.Applying != nil || st.Active != nil || len(st.Debts) != 0 || st.LastRelease != nil {
		return ErrCorruptState
	}
	return s.publish(st, "state.json", true)
}

func (s *Store) publish(value any, name string, noReplace bool) (err error) {
	var data []byte
	if body, ok := value.([]byte); ok {
		data = body
	} else {
		data, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	data = append(data, '\n')
	if err := s.safeRoot(); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.root, ".host-*")
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		if e := os.Remove(f.Name()); e != nil && !errors.Is(e, os.ErrNotExist) && err == nil {
			err = e
		}
	}()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err = s.trustCheck(f.Name(), info); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = s.syncFile(f); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	path := filepath.Join(s.root, name)
	if noReplace {
		err = s.link(f.Name(), path)
	} else {
		err = s.rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	return s.syncDir(s.root)
}

func (s *Store) syncState(st HostState) error {
	f, err := s.openFile("state.json")
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	current, err := decodeState(data)
	if err != nil {
		return err
	}
	if current.HostID != st.HostID || current.Journal.HighWater != st.Journal.HighWater || !reflect.DeepEqual(current.Journal.Last, st.Journal.Last) {
		return ErrRequestConflict
	}
	if err := s.syncFile(f); err != nil {
		return err
	}
	return s.syncDir(s.root)
}

func (s *Store) Submit(req Request) (*Receipt, error) {
	data, err := encodeRequest(req)
	if err != nil {
		return nil, err
	}
	digest := requestDigest(data)
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	disposition, err := checkRequest(st, req, digest)
	if err != nil {
		return nil, err
	}
	if disposition == RequestReplay {
		if err := s.syncState(st); err != nil {
			return nil, err
		}
		receipt := *st.Journal.Last
		return &receipt, nil
	}
	pending, err := s.TakeRequest()
	if err != nil {
		return nil, err
	}
	if pending != nil {
		return nil, s.matchPending(req, digest, *pending)
	}
	if err := s.publish(data, "request.json", true); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		winner, readErr := s.TakeRequest()
		if readErr != nil {
			return nil, readErr
		}
		if winner == nil {
			return nil, err
		}
		return nil, s.matchPending(req, digest, *winner)
	}
	return nil, nil
}

func (s *Store) matchPending(req Request, digest string, pending Request) error {
	saved, err := RequestDigest(pending)
	if err != nil {
		return err
	}
	if req.ID != pending.ID || digest != saved {
		return ErrRequestConflict
	}
	f, err := s.openFile("request.json")
	if err != nil {
		return err
	}
	defer f.Close()
	if err := s.syncFile(f); err != nil {
		return err
	}
	return s.syncDir(s.root)
}

func (s *Store) TakeRequest() (*Request, error) {
	f, err := s.openFile("request.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	req, err := DecodeControlRequest(f)
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *Store) CleanupRequest(req Request) error {
	st, err := s.Load()
	if err != nil {
		return err
	}
	disposition, checkErr := CheckRequest(st, req)
	if checkErr == nil {
		if disposition != RequestReplay {
			return ErrRequestConflict
		}
		if err := s.syncState(st); err != nil {
			return err
		}
	} else if !errors.Is(checkErr, ErrStaleRequest) && !errors.Is(checkErr, ErrRequestConflict) && !errors.Is(checkErr, ErrOutOfOrder) && !errors.Is(checkErr, ErrHostMismatch) {
		return checkErr
	}
	pending, err := s.TakeRequest()
	if err != nil {
		return err
	}
	if pending == nil {
		return s.syncDir(s.root)
	}
	digest, err := RequestDigest(req)
	if err != nil {
		return err
	}
	saved, err := RequestDigest(*pending)
	if err != nil {
		return err
	}
	if pending.ID != req.ID || digest != saved {
		return ErrRequestConflict
	}
	if err := os.Remove(filepath.Join(s.root, "request.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return s.syncDir(s.root)
}
