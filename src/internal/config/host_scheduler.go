package config

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
	"slices"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

type RepoRef struct {
	Owner string `yaml:"owner" json:"owner"`
	Name  string `yaml:"name" json:"name"`
	ID    int64  `yaml:"id" json:"id"`
}
type CredentialRef struct {
	ID            string    `yaml:"id" json:"id"`
	ResourceOwner string    `yaml:"resource_owner" json:"resource_owner"`
	TokenEnv      string    `yaml:"token_env" json:"token_env"`
	TokenFile     string    `yaml:"token_file" json:"token_file"`
	EnvFile       string    `yaml:"env_file" json:"env_file"`
	Repositories  []RepoRef `yaml:"repositories" json:"repositories"`
}
type RouteProof struct {
	WorkflowRef    string   `yaml:"workflow_ref" json:"workflow_ref"`
	Labels         []string `yaml:"labels" json:"labels"`
	GroupID        int64    `yaml:"group_id" json:"group_id"`
	EvidenceDigest string   `yaml:"evidence_digest" json:"evidence_digest"`
}
type DaemonRef struct {
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	UID      int    `yaml:"uid" json:"uid"`
	Rootless bool   `yaml:"rootless" json:"rootless"`
}
type EntryRef struct {
	Kind           string `yaml:"kind" json:"kind"`
	Name           string `yaml:"name" json:"name"`
	EvidenceDigest string `yaml:"evidence_digest" json:"evidence_digest"`
}
type HostInventory struct {
	Daemons     []DaemonRef `yaml:"daemons" json:"daemons"`
	Entries     []EntryRef  `yaml:"entries" json:"entries"`
	AuditDigest string      `yaml:"audit_digest" json:"audit_digest"`
}
type ResourceBudget struct {
	OuterBytes     uint64 `yaml:"outer_bytes" json:"outer_bytes"`
	InnerBytes     uint64 `yaml:"inner_bytes" json:"inner_bytes"`
	OuterPeak      uint64 `yaml:"outer_peak" json:"outer_peak"`
	InnerPeak      uint64 `yaml:"inner_peak" json:"inner_peak"`
	BaselineBytes  uint64 `yaml:"baseline_bytes" json:"baseline_bytes"`
	HostBytes      uint64 `yaml:"host_bytes" json:"host_bytes"`
	CPUs           string `yaml:"cpus" json:"cpus"`
	EvidenceDigest string `yaml:"evidence_digest" json:"evidence_digest"`
}
type HostSchedulerConfig struct {
	Mode                 string        `yaml:"mode" json:"mode"`
	OuterDockerEndpoint  string        `yaml:"outer_docker_endpoint" json:"outer_docker_endpoint"`
	PollIntervalSeconds  int           `yaml:"poll_interval_seconds" json:"poll_interval_seconds"`
	IdleRunnerTTLSeconds int           `yaml:"idle_runner_ttl_seconds" json:"idle_runner_ttl_seconds"`
	Inventory            HostInventory `yaml:"inventory" json:"inventory"`
}
type ProfileSchedulerConfig struct {
	Enabled      bool             `yaml:"enabled" json:"enabled"`
	Repositories []RepoRef        `yaml:"repositories" json:"repositories"`
	Routes       []RouteProof     `yaml:"routes" json:"routes"`
	EvidenceFile string           `yaml:"evidence_file" json:"evidence_file"`
	Budget       ResourceBudget   `yaml:"budget" json:"budget"`
	Execution    ExecutionBinding `yaml:"execution" json:"execution"`
}
type ParticipationBinding struct {
	Source       string `yaml:"source" json:"source"`
	ConfigDigest string `yaml:"config_digest" json:"config_digest"`
}
type BootstrapParameters struct {
	TargetURL string   `yaml:"target_url" json:"target_url"`
	Workdir   string   `yaml:"workdir" json:"workdir"`
	GroupName string   `yaml:"group_name" json:"group_name"`
	Labels    []string `yaml:"labels" json:"labels"`
	Ephemeral bool     `yaml:"ephemeral" json:"ephemeral"`
}
type ExecutionBinding struct {
	Outer           DaemonRef `yaml:"outer" json:"outer"`
	Inner           DaemonRef `yaml:"inner" json:"inner"`
	MountRefs       []string  `yaml:"mount_refs" json:"mount_refs"`
	EvidenceIDs     []string  `yaml:"evidence_ids" json:"evidence_ids"`
	InventoryDigest string    `yaml:"inventory_digest" json:"inventory_digest"`
}
type RuntimeSnapshot struct {
	Source         string              `yaml:"source" json:"source"`
	ConfigDigest   string              `yaml:"config_digest" json:"config_digest"`
	Target         ResolvedTarget      `yaml:"target" json:"target"`
	Credential     CredentialRef       `yaml:"credential" json:"credential"`
	ImageDigest    string              `yaml:"image_digest" json:"image_digest"`
	EvidenceFile   string              `yaml:"evidence_file" json:"evidence_file"`
	EvidenceDigest string              `yaml:"evidence_digest" json:"evidence_digest"`
	Bootstrap      BootstrapParameters `yaml:"bootstrap" json:"bootstrap"`
	Budget         ResourceBudget      `yaml:"budget" json:"budget"`
	Execution      ExecutionBinding    `yaml:"execution" json:"execution"`
	Repositories   []RepoRef           `yaml:"repositories" json:"repositories"`
	Routes         []RouteProof        `yaml:"routes" json:"routes"`
}
type CurrentProfiles struct {
	Global   GlobalConfig       `yaml:"global" json:"global"`
	Profiles []Profile          `yaml:"profiles" json:"profiles"`
	Errors   []ProfileLoadError `yaml:"errors" json:"errors"`
}

func ValidateBudget(b ResourceBudget) error {
	if b.HostBytes == 0 || b.CPUs == "" || b.EvidenceDigest == "" {
		return errors.New("budget evidence missing")
	}
	for _, pair := range []struct{ peak, limit uint64 }{{b.OuterPeak, b.OuterBytes}, {b.InnerPeak, b.InnerBytes}} {
		if pair.peak == 0 {
			return errors.New("layer peak missing")
		}
		margin := pair.peak / 5
		if pair.peak%5 != 0 {
			margin++
		}
		if pair.peak > ^uint64(0)-margin || pair.limit < pair.peak+margin {
			return errors.New("layer budget insufficient")
		}
	}
	total := b.BaselineBytes
	for _, n := range []uint64{b.OuterBytes, b.InnerBytes} {
		if total > ^uint64(0)-n {
			return errors.New("budget overflow")
		}
		total += n
	}
	if total > b.HostBytes {
		return errors.New("host capacity exceeded")
	}
	return nil
}

func ValidateHostConfig(g GlobalConfig, profiles []Profile) error {
	if err := validateHostGlobal(g); err != nil {
		return err
	}
	if err := validateIdentities(profiles); err != nil {
		return err
	}
	for _, p := range profiles {
		if err := validateHostProfile(g, p); err != nil {
			return fmt.Errorf("profile %s: %w", p.Name, err)
		}
	}
	return nil
}

func validateHostGlobal(g GlobalConfig) error {
	s := g.Scheduler
	if s.Mode != "disabled" && s.Mode != "host-single" {
		return errors.New("unsupported scheduler.mode")
	}
	if s.PollIntervalSeconds < 15 || s.PollIntervalSeconds > 30 || s.IdleRunnerTTLSeconds < 120 || s.IdleRunnerTTLSeconds > 300 {
		return errors.New("scheduler interval out of range")
	}
	if s.Mode == "disabled" {
		return nil
	}
	if !validEndpoint(s.OuterDockerEndpoint) || s.Inventory.AuditDigest == "" {
		return errors.New("explicit endpoint and inventory audit required")
	}
	seen := map[string]bool{}
	outer := false
	for _, d := range s.Inventory.Daemons {
		if !validDaemon(d) || seen[d.Endpoint] {
			return errors.New("invalid or duplicate daemon")
		}
		seen[d.Endpoint] = true
		if d.Endpoint == s.OuterDockerEndpoint && d.UID == 0 && !d.Rootless {
			outer = true
		}
	}
	if !outer {
		return errors.New("outer daemon missing from inventory")
	}
	entries := map[string]bool{}
	for _, e := range s.Inventory.Entries {
		key := e.Kind + ":" + e.Name
		if e.Kind == "" || e.Name == "" || e.EvidenceDigest == "" || entries[key] {
			return errors.New("invalid inventory entry")
		}
		entries[key] = true
	}
	return nil
}

func validateIdentities(profiles []Profile) error {
	for i, p := range profiles {
		for _, q := range profiles[:i] {
			if (p.Name != "" && p.Name == q.Name) || (p.Service.Name != "" && p.Service.Name == q.Service.Name) || (p.Source != "" && p.Source == q.Source) || prefixesOverlap(p.Docker.ContainerNamePrefix, q.Docker.ContainerNamePrefix) || prefixesOverlap(p.Runner.NamePrefix, q.Runner.NamePrefix) {
				return errors.New("duplicate or overlapping profile identity")
			}
		}
	}
	return nil
}
func prefixesOverlap(a, b string) bool {
	return a != "" && b != "" && (strings.HasPrefix(a, b) || strings.HasPrefix(b, a))
}

var repoOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
var envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var imagePattern = regexp.MustCompile(`^[^\s@]+@sha256:[a-f0-9]{64}$`)

// RepoKey is a repository key, not an authenticated observation key.
func (r RepoRef) RepoKey() string { return strings.ToLower(r.Owner + "/" + r.Name) }
func validRepo(r RepoRef) bool {
	return repoOwnerPattern.MatchString(r.Owner) && repoNamePattern.MatchString(r.Name) && r.Name != "." && r.Name != ".." && r.ID >= 0
}
func validAbsPath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsAny(p, "\x00\r\n")
}
func validEndpoint(p string) bool {
	return strings.HasPrefix(p, "unix://") && validAbsPath(strings.TrimPrefix(p, "unix://")) && strings.TrimPrefix(p, "unix://") != "/"
}
func validDaemon(d DaemonRef) bool {
	return validEndpoint(d.Endpoint) && d.UID >= 0 && (!d.Rootless || d.UID > 0)
}

func validateCredential(ref CredentialRef) error {
	if !isSafeManagedName(ref.ID) || !repoOwnerPattern.MatchString(ref.ResourceOwner) {
		return errors.New("credential identity/owner missing or invalid")
	}
	if ref.TokenFile == "" && ref.TokenEnv == "" && ref.EnvFile == "" {
		return errors.New("credential needs a token source")
	}
	if ref.TokenEnv != "" && !envNamePattern.MatchString(ref.TokenEnv) {
		return errors.New("invalid token environment reference")
	}
	if ref.TokenFile != "" && !validAbsPath(ref.TokenFile) || ref.EnvFile != "" && !validAbsPath(ref.EnvFile) {
		return errors.New("invalid credential file reference")
	}
	seen := map[string]bool{}
	for _, r := range ref.Repositories {
		if !validRepo(r) || seen[r.RepoKey()] {
			return errors.New("invalid credential repository scope")
		}
		seen[r.RepoKey()] = true
	}
	return nil
}
func profileCredential(g GlobalConfig, p Profile) CredentialRef {
	ref := CredentialRef{ID: p.GitHub.CredentialID, ResourceOwner: p.GitHub.ResourceOwner, TokenEnv: p.GitHub.TokenEnv, TokenFile: p.GitHub.TokenFile, EnvFile: p.GitHub.EnvFile, Repositories: p.Scheduler.Repositories}
	if ref.TokenFile == "" && ref.TokenEnv == "" && ref.EnvFile == "" {
		ref.TokenEnv = g.GitHub.TokenEnv
		ref.EnvFile = g.GitHub.EnvFile
	}
	return ref
}

func validateHostProfile(g GlobalConfig, p Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if !p.Scheduler.Enabled {
		return nil
	}
	if g.Scheduler.Mode != "host-single" {
		return errors.New("enabled profile requires host-single mode")
	}
	if err := validateRuntimeProfile(p); err != nil {
		return err
	}
	ref := profileCredential(g, p)
	if err := validateCredential(ref); err != nil {
		return err
	}
	if err := validateCredentialTarget(p, ref); err != nil {
		return err
	}
	blocked := []string{g.Paths.ProfilesDir, g.Paths.StateDir, g.Paths.LogDir, strings.TrimPrefix(g.Scheduler.OuterDockerEndpoint, "unix://")}
	for _, daemon := range g.Scheduler.Inventory.Daemons {
		blocked = append(blocked, strings.TrimPrefix(daemon.Endpoint, "unix://"))
	}
	for _, mount := range p.Scheduler.Execution.MountRefs {
		q := p
		q.GitHub.TokenFile = ref.TokenFile
		q.GitHub.EnvFile = ref.EnvFile
		if err := validateMount(mount, q, blocked...); err != nil {
			return err
		}
	}
	e := p.Scheduler.Execution
	if e.InventoryDigest != g.Scheduler.Inventory.AuditDigest || e.Outer.Endpoint != g.Scheduler.OuterDockerEndpoint || !slices.Contains(g.Scheduler.Inventory.Daemons, e.Outer) || !slices.Contains(g.Scheduler.Inventory.Daemons, e.Inner) {
		return errors.New("execution does not bind current inventory")
	}
	return nil
}

func validateCredentialTarget(p Profile, ref CredentialRef) error {
	target, err := p.ResolveTarget()
	if err != nil {
		return err
	}
	owner := target.Owner
	if target.Scope == TargetScopeOrganization {
		owner = target.OrgSlug
	}
	if !strings.EqualFold(ref.ResourceOwner, owner) {
		return errors.New("credential resource owner does not match target")
	}
	return nil
}

func validateRuntimeProfile(p Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if !p.Runner.Ephemeral || !isSafeManagedName(p.Runner.NamePrefix) || !validAbsPath(p.Runner.Workdir) || !imagePattern.MatchString(p.Docker.Image) || !validAbsPath(p.Scheduler.EvidenceFile) {
		return errors.New("unfrozen runner/image/evidence parameters")
	}
	if p.Docker.AccessMode != DockerAccessModeRootless || len(p.Docker.Env) > 0 || len(p.Docker.Volumes) > 0 {
		return errors.New("host-socket or unmodeled Docker environment/mounts forbidden")
	}
	if err := ValidateBudget(p.Scheduler.Budget); err != nil {
		return err
	}
	if p.Docker.CPUs != "" && p.Docker.CPUs != p.Scheduler.Budget.CPUs || p.Docker.Memory != "" {
		return errors.New("unmodeled Docker resource override")
	}
	target, err := p.ResolveTarget()
	if err != nil {
		return err
	}
	if len(p.Scheduler.Repositories) == 0 {
		return errors.New("explicit repository scope required")
	}
	seen := map[string]bool{}
	for _, r := range p.Scheduler.Repositories {
		if !validRepo(r) || seen[r.RepoKey()] {
			return errors.New("invalid monitored repository")
		}
		seen[r.RepoKey()] = true
		if target.Scope == TargetScopeRepository && (!strings.EqualFold(r.Owner, target.Owner) || !strings.EqualFold(r.Name, target.Repo)) {
			return errors.New("personal scope cannot expand")
		}
		if target.Scope == TargetScopeOrganization && !strings.EqualFold(r.Owner, target.OrgSlug) {
			return errors.New("organization repository outside target")
		}
	}
	if target.Scope == TargetScopeRepository && len(seen) != 1 {
		return errors.New("personal scope must be exact")
	}
	if len(p.Scheduler.Routes) == 0 {
		return errors.New("route evidence required")
	}
	for _, r := range p.Scheduler.Routes {
		if r.WorkflowRef == "" || len(r.Labels) == 0 || r.GroupID < 0 || r.EvidenceDigest == "" {
			return errors.New("route evidence incomplete")
		}
		for _, l := range r.Labels {
			if strings.TrimSpace(l) == "" {
				return errors.New("empty route label")
			}
		}
	}
	e := p.Scheduler.Execution
	if !validDaemon(e.Outer) || !validDaemon(e.Inner) || e.Outer.UID != 0 || e.Outer.Rootless || !e.Inner.Rootless || e.Outer.Endpoint == e.Inner.Endpoint || e.InventoryDigest == "" || len(e.EvidenceIDs) == 0 {
		return errors.New("execution evidence incomplete")
	}
	for _, id := range e.EvidenceIDs {
		if strings.TrimSpace(id) == "" {
			return errors.New("empty execution evidence")
		}
	}
	for _, mount := range e.MountRefs {
		if err := validateMount(mount, p); err != nil {
			return err
		}
	}
	return nil
}

func validateMount(mount string, p Profile, controlPaths ...string) error {
	parts := strings.Split(mount, ":")
	if len(parts) < 2 || len(parts) > 3 || !validAbsPath(parts[0]) || !validAbsPath(parts[1]) || len(parts) == 3 && parts[2] != "ro" && parts[2] != "rw" {
		return errors.New("invalid audited mount reference")
	}
	blockedPaths := append([]string{"/etc/gha-runner-tui", "/var/lib/gha-runner-tui", "/var/run", "/run", p.GitHub.TokenFile, p.GitHub.EnvFile, p.Scheduler.EvidenceFile, strings.TrimPrefix(p.Scheduler.Execution.Outer.Endpoint, "unix://"), strings.TrimPrefix(p.Scheduler.Execution.Inner.Endpoint, "unix://")}, controlPaths...)
	if p.Source != "" {
		blockedPaths = append(blockedPaths, filepath.Dir(p.Source))
	}
	for _, path := range parts[:2] {
		for _, blocked := range blockedPaths {
			if blocked != "" && (path == blocked || strings.HasPrefix(path, blocked+"/") || strings.HasPrefix(blocked, path+"/") || path == "/") {
				return errors.New("credential/control/socket mount forbidden")
			}
		}
		if strings.HasSuffix(path, ".sock") {
			return errors.New("socket mount forbidden")
		}
	}
	return nil
}

// strictYAML rejects unmodeled behavior rather than silently dropping it from a digest.
func strictYAML(data []byte, out any) error {
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("multiple YAML documents forbidden")
	}
	return nil
}
func readHostProfile(path string) (Profile, error) {
	if err := noSymlinks(path); err != nil {
		return Profile{}, err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return Profile{}, err
	}
	if !st.Mode().IsRegular() {
		return Profile{}, errors.New("profile must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err := strictYAML(data, &p); err != nil {
		return Profile{}, err
	}
	p.Source = path
	return p, nil
}
func profileDigest(p Profile) (string, error) {
	if len(p.Docker.Env) > 0 {
		return "", errors.New("unmodeled environment cannot be frozen or hashed")
	}
	p.Scheduler.Enabled = false
	p.Source = ""
	data, err := yaml.Marshal(p)
	if err != nil {
		return "", err
	}
	var canonical any
	if err := yaml.Unmarshal(data, &canonical); err != nil {
		return "", err
	}
	data, err = json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func confinedSource(dir, source string) error {
	if !validAbsPath(dir) || !validAbsPath(source) || filepath.Dir(source) != dir || (filepath.Ext(source) != ".yaml" && filepath.Ext(source) != ".yml") {
		return errors.New("profile source outside bound flat directory")
	}
	if err := noSymlinks(dir); err != nil {
		return err
	}
	return noSymlinks(source)
}
func noSymlinks(path string) error {
	if !validAbsPath(path) {
		return errors.New("canonical absolute path required")
	}
	for current := path; ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlink forbidden")
		}
		if current == "/" {
			break
		}
	}
	return nil
}

func FreezeParticipation(profilesDir string, p Profile) (ParticipationBinding, error) {
	if err := confinedSource(profilesDir, p.Source); err != nil {
		return ParticipationBinding{}, err
	}
	current, err := readHostProfile(p.Source)
	if err != nil {
		return ParticipationBinding{}, err
	}
	if err := current.Validate(); err != nil {
		return ParticipationBinding{}, err
	}
	digest, err := profileDigest(current)
	if err != nil {
		return ParticipationBinding{}, err
	}
	supplied, err := profileDigest(p)
	if err != nil {
		return ParticipationBinding{}, err
	}
	if supplied != digest {
		return ParticipationBinding{}, errors.New("profile changed before freezing")
	}
	return ParticipationBinding{Source: p.Source, ConfigDigest: digest}, nil
}

// Callers hold the fixed host lock across configuration writers and recovery.
func SaveParticipationAt(profilesDir, profileID string, binding ParticipationBinding, enabled bool) error {
	if !isSafeManagedName(profileID) || binding.ConfigDigest == "" {
		return errors.New("invalid participation binding")
	}
	if err := confinedSource(profilesDir, binding.Source); err != nil {
		return err
	}
	p, err := readHostProfile(binding.Source)
	if err != nil {
		return err
	}
	if p.Name != profileID {
		return errors.New("profile identity changed")
	}
	frozen, err := FreezeParticipation(profilesDir, p)
	if err != nil {
		return err
	}
	if frozen != binding {
		return errors.New("profile binding changed")
	}
	data, err := os.ReadFile(binding.Source)
	if err != nil {
		return err
	}
	var document yaml.Node
	if err := yaml.Unmarshal(data, &document); err != nil {
		return err
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return errors.New("profile must be a YAML mapping")
	}
	root := document.Content[0]
	scheduler := mappingValue(root, "scheduler")
	if scheduler == nil {
		scheduler = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "scheduler"}, scheduler)
	}
	if scheduler.Kind != yaml.MappingNode || scheduler.Anchor != "" {
		return errors.New("unsupported participation scheduler YAML shape")
	}
	field := mappingValue(scheduler, "enabled")
	if field == nil {
		field = &yaml.Node{Kind: yaml.ScalarNode}
		scheduler.Content = append(scheduler.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "enabled"}, field)
	}
	if field.Kind != yaml.ScalarNode || field.Anchor != "" {
		return errors.New("unsupported participation enabled YAML shape")
	}
	if err := field.Encode(enabled); err != nil {
		return err
	}
	output, err := yaml.Marshal(&document)
	if err != nil {
		return err
	}
	// Validate the generated document, not just the old source, before publication.
	var generated Profile
	if err := strictYAML(output, &generated); err != nil {
		return err
	}
	if err := generated.Validate(); err != nil {
		return err
	}
	digest, err := profileDigest(generated)
	if err != nil {
		return err
	}
	if generated.Scheduler.Enabled != enabled || digest != binding.ConfigDigest {
		return errors.New("participation output changed bound configuration")
	}
	tmp, err := os.CreateTemp(profilesDir, ".participation-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err = tmp.Write(output); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// Recheck source and content immediately before replacing it under the caller's lock.
	now, err := readHostProfile(binding.Source)
	if err != nil {
		return err
	}
	again, err := FreezeParticipation(profilesDir, now)
	if err != nil {
		return err
	}
	if again != binding {
		return errors.New("profile changed before publication")
	}
	if err = os.Rename(tmp.Name(), binding.Source); err != nil {
		return err
	}
	directory, err := os.Open(profilesDir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func checkPathTrust(path string, uid uint32) error {
	if err := noSymlinks(path); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid || st.Mode().Perm()&0022 != 0 || (!st.IsDir() && !st.Mode().IsRegular()) {
		return errors.New("unsafe configuration owner/mode")
	}
	return nil
}
func productionRootTrust(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		if err := checkPathTrust(current, 0); err != nil {
			return err
		}
		if current == "/" {
			break
		}
	}
	return nil
}
func LoadCurrent(canonicalConfigPath, boundProfilesDir string) (CurrentProfiles, error) {
	return loadCurrentWithTrust(canonicalConfigPath, boundProfilesDir, productionRootTrust)
}
func loadCurrentWithTrust(canonicalConfigPath, boundProfilesDir string, checkRoot func(string) error) (CurrentProfiles, error) {
	if checkRoot == nil {
		return CurrentProfiles{}, errors.New("root trust checker required")
	}
	for _, path := range []string{canonicalConfigPath, boundProfilesDir} {
		if err := noSymlinks(path); err != nil {
			return CurrentProfiles{}, err
		}
		if err := checkRoot(path); err != nil {
			return CurrentProfiles{}, err
		}
	}
	data, err := os.ReadFile(canonicalConfigPath)
	if err != nil {
		return CurrentProfiles{}, err
	}
	g := DefaultGlobalConfig()
	if err := strictYAML(data, &g); err != nil {
		return CurrentProfiles{}, err
	}
	g.applyDefaults()
	if g.Paths.ProfilesDir != boundProfilesDir {
		return CurrentProfiles{}, errors.New("profiles directory binding changed")
	}
	if err := validateHostGlobal(g); err != nil {
		return CurrentProfiles{}, err
	}
	entries, err := os.ReadDir(boundProfilesDir)
	if err != nil {
		return CurrentProfiles{}, err
	}
	result := CurrentProfiles{Global: g}
	var identities []Profile
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".yaml") && !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}
		path := filepath.Join(boundProfilesDir, entry.Name())
		err := checkRoot(path)
		var p Profile
		if err == nil {
			p, err = readHostProfile(path)
		}
		if err == nil {
			identities = append(identities, p)
		}
		if err == nil {
			err = validateHostProfile(g, p)
		}
		if err == nil && p.Scheduler.Enabled {
			for _, mount := range p.Scheduler.Execution.MountRefs {
				if err = validateMount(mount, p, filepath.Dir(canonicalConfigPath), boundProfilesDir); err != nil {
					break
				}
			}
		}
		// A modeled credential reference is required even for a dormant current profile.
		if err == nil {
			err = validateCredential(profileCredential(g, p))
		}
		if err == nil {
			err = validateCredentialTarget(p, profileCredential(g, p))
		}
		if err != nil {
			result.Errors = append(result.Errors, ProfileLoadError{Path: path, Err: err})
			continue
		}
		result.Profiles = append(result.Profiles, p)
	}
	if err := validateIdentities(identities); err != nil {
		return CurrentProfiles{}, err
	}
	return result, nil
}
func (c CurrentProfiles) Find(profileID string) (Profile, error) {
	var found *Profile
	for _, p := range c.Profiles {
		if p.Name == profileID {
			if found != nil {
				return Profile{}, errors.New("duplicate profile identity")
			}
			copy := p
			found = &copy
		}
	}
	if found == nil {
		return Profile{}, fmt.Errorf("profile %s unavailable", profileID)
	}
	return *found, nil
}
func FreezeRuntime(profilesDir string, p Profile, ref CredentialRef, evidenceDigest string, effectiveLabels []string) (RuntimeSnapshot, error) {
	if err := validateRuntimeProfile(p); err != nil {
		return RuntimeSnapshot{}, err
	}
	if err := validateCredential(ref); err != nil {
		return RuntimeSnapshot{}, err
	}
	if err := validateCredentialTarget(p, ref); err != nil {
		return RuntimeSnapshot{}, err
	}
	explicitSource := p.GitHub.TokenEnv != "" || p.GitHub.TokenFile != "" || p.GitHub.EnvFile != ""
	if ref.ID != p.GitHub.CredentialID || ref.ResourceOwner != p.GitHub.ResourceOwner || explicitSource && (ref.TokenEnv != p.GitHub.TokenEnv || ref.TokenFile != p.GitHub.TokenFile || ref.EnvFile != p.GitHub.EnvFile) || !reflect.DeepEqual(ref.Repositories, p.Scheduler.Repositories) {
		return RuntimeSnapshot{}, errors.New("credential does not bind profile")
	}
	if evidenceDigest == "" || len(effectiveLabels) == 0 {
		return RuntimeSnapshot{}, errors.New("verified image labels/evidence required")
	}
	for _, l := range effectiveLabels {
		if strings.TrimSpace(l) == "" {
			return RuntimeSnapshot{}, errors.New("empty effective label")
		}
	}
	for _, mount := range p.Scheduler.Execution.MountRefs {
		q := p
		q.GitHub.TokenFile = ref.TokenFile
		q.GitHub.EnvFile = ref.EnvFile
		if err := validateMount(mount, q, profilesDir); err != nil {
			return RuntimeSnapshot{}, err
		}
	}
	binding, err := FreezeParticipation(profilesDir, p)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	target, err := p.ResolveTarget()
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	ref.Repositories = slices.Clone(ref.Repositories)
	execution := p.Scheduler.Execution
	execution.MountRefs = slices.Clone(execution.MountRefs)
	execution.EvidenceIDs = slices.Clone(execution.EvidenceIDs)
	routes := slices.Clone(p.Scheduler.Routes)
	for i := range routes {
		routes[i].Labels = slices.Clone(routes[i].Labels)
	}
	return RuntimeSnapshot{Source: binding.Source, ConfigDigest: binding.ConfigDigest, Target: target, Credential: ref, ImageDigest: p.Docker.Image, EvidenceFile: p.Scheduler.EvidenceFile, EvidenceDigest: evidenceDigest, Bootstrap: BootstrapParameters{TargetURL: target.GitHubURL(), Workdir: p.Runner.Workdir, GroupName: p.RunnerGroup.Name, Labels: slices.Clone(effectiveLabels), Ephemeral: p.Runner.Ephemeral}, Budget: p.Scheduler.Budget, Execution: execution, Repositories: slices.Clone(p.Scheduler.Repositories), Routes: routes}, nil
}
