package instaboxcontroller

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	// hardRunCap is the controller's own ceiling: the platform may grow
	// maxRunMinutes during a free-plan run, but the delivered deadline never
	// moves past the initial two hours of the run.
	hardRunCap = 120 * time.Minute
	// admissionFloor mirrors the loop's session deadline floor (90 minutes of
	// work plus a 10 minute buffer).
	admissionFloor = 100 * time.Minute
	// standbyWakeThreshold wakes a standby once the only usable machine has
	// five minutes less than the admission floor left.
	standbyWakeThreshold = 105 * time.Minute
	// The usable budget window of a run is the 20 minutes between the
	// admission floor and the hard cap.
	defaultPollInterval = 30 * time.Second
	minPollInterval     = 10 * time.Second
	maxPollInterval     = time.Hour

	stateVersion = 1

	// DefaultAPIKeyEnv is the environment variable used by a machine that does
	// not name its own apiKeyEnv, preserving old single-account configs.
	DefaultAPIKeyEnv = "INSTABOX_API_KEY"
)

var (
	machineIDPattern = regexp.MustCompile(`^m_[a-z2-7]{26}$`)
	sshHostPattern   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	sshUserPattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]{0,31}$`)
	apiKeyEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// Config is the controller's local JSON configuration. It intentionally has no
// API base URL and no raw API key field: each machine may name the environment
// variable holding the key for its Instabox account, and the API host is fixed.
type Config struct {
	PollIntervalSeconds int             `json:"pollIntervalSeconds"`
	StateDir            string          `json:"stateDir"`
	EnableRemoteWrite   bool            `json:"enableRemoteWrite"`
	EnableWake          bool            `json:"enableWake"`
	Machines            []MachineConfig `json:"machines"`
}

// MachineConfig describes one machine and the absolute deadline file on it.
// APIKeyEnv optionally names the environment variable holding the API key for
// this machine's Instabox account; nil keeps the legacy INSTABOX_API_KEY.
type MachineConfig struct {
	MachineID       string  `json:"machineId"`
	DeadlinePath    string  `json:"deadlinePath"`
	SSHHost         string  `json:"sshHost"`
	SSHUser         string  `json:"sshUser"`
	SSHPort         int     `json:"sshPort"`
	SSHIdentityFile string  `json:"sshIdentityFile"`
	APIKeyEnv       *string `json:"apiKeyEnv,omitempty"`
}

// UnmarshalJSON accepts apiKeyEnv only as a string name. A present but null
// field is rejected instead of silently falling back to the global key.
func (m *MachineConfig) UnmarshalJSON(data []byte) error {
	type plain MachineConfig
	wrapped := struct {
		*plain
		APIKeyEnv json.RawMessage `json:"apiKeyEnv"`
	}{plain: (*plain)(m)}
	m.APIKeyEnv = nil
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wrapped); err != nil {
		return err
	}
	if len(wrapped.APIKeyEnv) == 0 {
		return nil
	}
	var name string
	if err := json.Unmarshal(wrapped.APIKeyEnv, &name); err != nil {
		return errors.New("apiKeyEnv must be a string environment variable name")
	}
	m.APIKeyEnv = &name
	return nil
}

// LoadConfig reads and strictly validates the local configuration. Unknown
// fields are rejected so a typo cannot silently disable a safeguard.
func LoadConfig(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, errors.New("controller config could not be opened")
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, errors.New("controller config is not valid JSON for this schema")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("controller config has trailing data")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// PollInterval returns the configured poll cadence, defaulting to 30 seconds.
func (c Config) PollInterval() time.Duration {
	if c.PollIntervalSeconds == 0 {
		return defaultPollInterval
	}
	return time.Duration(c.PollIntervalSeconds) * time.Second
}

// Validate rejects unsafe machine IDs, paths and SSH parameters before any
// request or SSH connection. It holds no secrets and prints none.
func (c *Config) Validate() error {
	if len(c.Machines) == 0 {
		return errors.New("controller config must list at least one machine")
	}
	if c.PollIntervalSeconds != 0 {
		interval := time.Duration(c.PollIntervalSeconds) * time.Second
		if interval < minPollInterval || interval > maxPollInterval {
			return errors.New("pollIntervalSeconds must be between 10 and 3600")
		}
	}
	if !validLocalPath(c.StateDir) {
		return errors.New("stateDir must be an absolute, clean path with safe characters")
	}
	seen := map[string]bool{}
	for i := range c.Machines {
		m := &c.Machines[i]
		if err := m.Validate(); err != nil {
			return err
		}
		if seen[m.MachineID] {
			return errors.New("controller config lists a machine more than once")
		}
		seen[m.MachineID] = true
	}
	return nil
}

// Validate checks one machine entry.
func (m *MachineConfig) Validate() error {
	if !machineIDPattern.MatchString(m.MachineID) {
		return errors.New("machineId must be an Instabox machine ID (m_...)")
	}
	if m.APIKeyEnv != nil && !apiKeyEnvPattern.MatchString(*m.APIKeyEnv) {
		return errors.New("apiKeyEnv must be an environment variable name, not a key value")
	}
	if !validLocalPath(m.DeadlinePath) {
		return errors.New("deadlinePath must be absolute, clean and use safe characters")
	}
	if !sshHostPattern.MatchString(m.SSHHost) {
		return errors.New("sshHost must be a literal hostname or IPv4 address")
	}
	if !sshUserPattern.MatchString(m.SSHUser) {
		return errors.New("sshUser is not a valid login name")
	}
	if m.SSHPort == 0 {
		m.SSHPort = 22
	}
	if m.SSHPort < 1 || m.SSHPort > 65535 {
		return errors.New("sshPort must be between 1 and 65535")
	}
	if m.SSHIdentityFile != "" && !validLocalPath(m.SSHIdentityFile) {
		return errors.New("sshIdentityFile must be an absolute, clean path")
	}
	return nil
}

// validLocalPath keeps shell quoting and remote replacement predictable.
func validLocalPath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	if len(path) > 4096 {
		return false
	}
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-', r == '/':
		default:
			return false
		}
	}
	return path != "/"
}

// budget is the controller's own view of a run. It is derived only from a GET
// response; stored state never extends it.
type budget struct {
	RunStartedAt time.Time
	Deadline     time.Time
}

func (b budget) remaining(now time.Time) time.Duration { return b.Deadline.Sub(now) }
func (b budget) runKey() string {
	return b.RunStartedAt.UTC().Format(time.RFC3339Nano)
}
func (b budget) deadlineKey() string {
	return b.Deadline.UTC().Format(time.RFC3339)
}

// computeBudget caps a run at runStartedAt + min(maxRunMinutes, 120 minutes).
// A missing or untrusted start or max-run value makes the machine not ready:
// the controller never guesses a longer budget.
func computeBudget(m Machine, now time.Time) (budget, error) {
	if m.State != "running" {
		return budget{}, fmt.Errorf("machine is %s, not running", m.State)
	}
	if !m.Runtime.Ready {
		return budget{}, errors.New("machine runtime is not ready")
	}
	if m.Runtime.HostUnreachable {
		return budget{}, errors.New("machine host is unreachable")
	}
	if m.RunStartedAt == nil || strings.TrimSpace(*m.RunStartedAt) == "" {
		return budget{}, errors.New("machine has no runStartedAt")
	}
	started, err := parseAPITime(*m.RunStartedAt)
	if err != nil {
		return budget{}, errors.New("machine runStartedAt is not a valid time")
	}
	if started.After(now.Add(2 * time.Minute)) {
		return budget{}, errors.New("machine runStartedAt is in the future")
	}
	if m.MaxRunMinutes == nil {
		return budget{}, errors.New("machine has no maxRunMinutes")
	}
	limit := hardRunCap
	switch max := *m.MaxRunMinutes; {
	case max < -1:
		return budget{}, errors.New("machine maxRunMinutes is invalid")
	case max == -1:
		// Unlimited on the platform; the local hard cap still applies.
	case max == 0:
		limit = 0
	case max < 120:
		limit = time.Duration(max) * time.Minute
	}
	return budget{
		RunStartedAt: started.UTC(),
		Deadline:     started.Add(limit).UTC().Truncate(time.Second),
	}, nil
}

func parseAPITime(value string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, value)
}

// budgetReady requires the fresh GET to leave a run in the 20-minute usable
// window between the admission floor and the 120-minute cap.
func budgetReady(b budget, now time.Time) bool {
	remaining := b.remaining(now)
	return remaining >= admissionFloor && remaining <= hardRunCap
}

func deadlinePayload(deadline time.Time) string {
	body, _ := json.Marshal(struct {
		Deadline string `json:"deadline"`
	}{Deadline: deadline.UTC().Format(time.RFC3339)})
	return string(body) + "\n"
}

// deliveryState records one machine + remote file target. Status is "pending"
// when a write may have happened but was not read back, and "confirmed" only
// after an exact readback.
type deliveryState struct {
	Target       string     `json:"target"`
	RunStartedAt string     `json:"runStartedAt"`
	Deadline     string     `json:"deadline"`
	Status       string     `json:"status"`
	AttemptedAt  time.Time  `json:"attemptedAt"`
	ConfirmedAt  *time.Time `json:"confirmedAt,omitempty"`
}

type wakeAttempt struct {
	AttemptedAt time.Time `json:"attemptedAt"`
	Status      string    `json:"status"`
}

type controllerState struct {
	Version      int                      `json:"version"`
	Deliveries   map[string]deliveryState `json:"deliveries"`
	WakeAttempts map[string]wakeAttempt   `json:"wakeAttempts"`
}

func emptyState() controllerState {
	return controllerState{
		Version:      stateVersion,
		Deliveries:   map[string]deliveryState{},
		WakeAttempts: map[string]wakeAttempt{},
	}
}

// Logger is the small logging surface the controller needs; standard
// log.Logger satisfies it.
type Logger interface {
	Printf(format string, args ...any)
}

// Controller polls the platform, delivers deadlines and, only when explicitly
// enabled, wakes one safe standby. APIs maps a validated machine ID to the
// client for that machine's account; a machine without a client is not ready
// and is never sent to another account's key.
type Controller struct {
	Config Config
	APIs   map[string]MachineAPI
	Writer DeadlineWriter
	DryRun bool
	Logger Logger
	Now    func() time.Time

	state          controllerState
	loaded         bool
	lock           *lockHandle
	roundGetFailed bool
}

// Close releases the single-instance lock.
func (c *Controller) Close() {
	if c.lock != nil {
		c.lock.release()
		c.lock = nil
	}
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *Controller) logf(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf(format, args...)
	}
}

// apiFor returns the client for exactly this machine's account. A missing
// client is an error, never a fallback to another machine's key.
func (c *Controller) apiFor(machineID string) (MachineAPI, error) {
	if api := c.APIs[machineID]; api != nil {
		return api, nil
	}
	return nil, fmt.Errorf("machine %s has no API client for its account", machineID)
}

// ensureReady creates the state directory, takes the lock and loads state
// before any network or SSH access. A corrupt state file, a machine without its
// own API client or a second instance fails closed here.
func (c *Controller) ensureReady() error {
	if c.loaded {
		return nil
	}
	if err := c.Config.Validate(); err != nil {
		return err
	}
	for _, machine := range c.Config.Machines {
		if c.APIs[machine.MachineID] == nil {
			return fmt.Errorf("machine %s has no API client for its account", machine.MachineID)
		}
	}
	if err := os.MkdirAll(c.Config.StateDir, 0o700); err != nil {
		return errors.New("controller state directory could not be created")
	}
	if c.lock == nil {
		lock, err := acquireLock(filepath.Join(c.Config.StateDir, "controller.lock"))
		if err != nil {
			return err
		}
		c.lock = lock
	}
	state, err := loadState(filepath.Join(c.Config.StateDir, "state.json"))
	if err != nil {
		return err
	}
	c.state = state
	c.loaded = true
	return nil
}

// Run polls until the context ends. Per-round failures are logged and retried;
// lock and state failures are fatal.
func (c *Controller) Run(ctx context.Context) error {
	if err := c.ensureReady(); err != nil {
		return err
	}
	interval := c.Config.PollInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := c.RunOnce(ctx); err != nil {
			c.logf("poll failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

type machineOutcome struct {
	config    MachineConfig
	machine   Machine
	hasResult bool
	getFailed bool
	state     string
	remaining time.Duration
	ready     bool
}

// RunOnce performs one whole polling round. It returns an error when a GET
// failed or a write/wake confirmation could not be persisted, so a --once run
// can fail visibly; the long-running loop keeps going.
func (c *Controller) RunOnce(ctx context.Context) error {
	if err := c.ensureReady(); err != nil {
		return err
	}
	now := c.now()
	outcomes := make([]machineOutcome, len(c.Config.Machines))
	allGETsOK := true
	c.roundGetFailed = false
	var problems []string

	for i, mc := range c.Config.Machines {
		outcome := machineOutcome{config: mc}
		api, err := c.apiFor(mc.MachineID)
		if err != nil {
			outcome.getFailed = true
			allGETsOK = false
			c.roundGetFailed = true
			problems = append(problems, err.Error())
			c.logf("%v", err)
			outcomes[i] = outcome
			continue
		}
		machine, err := api.GetMachine(ctx, mc.MachineID)
		if err != nil {
			outcome.getFailed = true
			allGETsOK = false
			c.roundGetFailed = true
			problems = append(problems, fmt.Sprintf("%s: %v", mc.MachineID, err))
			c.logf("machine %s: GET failed, no readiness and no wake this round: %v", mc.MachineID, err)
			outcomes[i] = outcome
			continue
		}
		outcome.machine = machine
		outcome.hasResult = true
		outcome.state = machine.State
		budget, err := computeBudget(machine, now)
		if err != nil {
			outcome.state = machine.State
			c.logf("machine %s: not budget-ready: %v", mc.MachineID, err)
			outcomes[i] = outcome
			continue
		}
		outcome.remaining = budget.remaining(now)
		if outcome.remaining <= 0 {
			// A trusted deadline at or before now is still authoritative. It
			// must replace a previously looser value on the machine even
			// though this run can no longer be budget-ready.
			c.logf("machine %s: run budget is exhausted (%s remaining); delivery still reconciled", mc.MachineID, outcome.remaining.Round(time.Second))
		}
		ready, err := c.ensureDelivery(ctx, mc, machine, budget, now)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", mc.MachineID, err))
			c.logf("machine %s: %v", mc.MachineID, err)
		}
		outcome.ready = ready
		c.logf("machine %s: state=%s remaining=%s budget-ready=%t", mc.MachineID, outcome.state, outcome.remaining.Round(time.Second), ready)
		outcomes[i] = outcome
	}

	if err := c.maybeWake(ctx, outcomes, allGETsOK, now); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// ensureDelivery makes the remote deadline match the current run, and reports
// whether a fresh GET now proves a delivered, in-window budget.
func (c *Controller) ensureDelivery(ctx context.Context, mc MachineConfig, machine Machine, budget budget, now time.Time) (bool, error) {
	key := mc.MachineID
	target := remoteTarget(mc)
	entry, exists := c.state.Deliveries[key]
	need, verify := deliveryNeeded(entry, exists, target, budget)

	if !need {
		// The GET at the start of this round is the fresh observation: an
		// earlier confirmation is only reused after it matches this run.
		return budgetReady(budget, now), nil
	}
	if !c.Config.EnableRemoteWrite || c.DryRun {
		c.logf("machine %s: deadline %s for this run is not delivered (remote writes disabled)", mc.MachineID, budget.Deadline.Format(time.RFC3339))
		return false, nil
	}
	if c.Writer == nil {
		return false, errors.New("remote deadline writer is not configured")
	}

	content := deadlinePayload(budget.Deadline)
	entry = deliveryState{
		Target:       target.key(),
		RunStartedAt: budget.runKey(),
		Deadline:     budget.deadlineKey(),
		Status:       "pending",
		AttemptedAt:  now,
	}
	c.state.Deliveries[key] = entry
	// The pending record lands before SSH: a crash can only cause a read-first
	// confirmation next time, never a blind overwrite.
	if err := c.saveState(); err != nil {
		return false, fmt.Errorf("deadline delivery state could not be saved: %w", err)
	}

	if verify {
		got, err := c.Writer.ReadDeadline(ctx, target)
		if err != nil {
			return false, fmt.Errorf("remote deadline remains unconfirmed: %w", err)
		}
		if sameDeadline(got, content) {
			ready, err := c.confirmDelivery(key, entry, now, budget)
			if err != nil {
				return false, err
			}
			// The readback confirmed an earlier write, but only a fresh GET can
			// prove a new run did not start while it was being confirmed.
			return c.verifyAfterConfirmation(ctx, mc, machine, budget, ready)
		}
		// Different content is a known state; the atomic replace below is safe.
	}

	got, err := c.Writer.WriteDeadline(ctx, target, content)
	if err != nil {
		return false, fmt.Errorf("remote deadline write failed and remains unconfirmed: %w", err)
	}
	if !sameDeadline(got, content) {
		return false, errors.New("remote deadline readback did not match the written value")
	}
	ready, err := c.confirmDelivery(key, entry, now, budget)
	if err != nil {
		return false, err
	}
	return c.verifyAfterConfirmation(ctx, mc, machine, budget, ready)
}

// verifyAfterConfirmation performs the post-confirmation GET that both the
// write and the pending-readback paths need. It proves the identity and
// deadline the confirmation assumed did not move in between; a changed run or
// deadline is never reported ready.
func (c *Controller) verifyAfterConfirmation(ctx context.Context, mc MachineConfig, machine Machine, budget budget, ready bool) (bool, error) {
	api, err := c.apiFor(mc.MachineID)
	if err != nil {
		c.roundGetFailed = true
		return false, errors.New("post-write GET has no API client for this machine's account; delivery is confirmed but readiness is not")
	}
	fresh, err := api.GetMachine(ctx, mc.MachineID)
	if err != nil {
		c.roundGetFailed = true
		return false, errors.New("post-write GET failed; delivery is confirmed but readiness is not")
	}
	if fresh.ID != machine.ID {
		return false, errors.New("post-write GET returned a different machine")
	}
	freshNow := c.now()
	freshBudget, err := computeBudget(fresh, freshNow)
	if err != nil {
		return false, fmt.Errorf("post-write GET is not usable: %w", err)
	}
	if freshBudget.runKey() != budget.runKey() || !freshBudget.Deadline.Equal(budget.Deadline) {
		return false, errors.New("post-write GET shows a different run or deadline")
	}
	return ready && budgetReady(freshBudget, freshNow), nil
}

func (c *Controller) confirmDelivery(key string, entry deliveryState, now time.Time, budget budget) (bool, error) {
	confirmed := now
	entry.Status = "confirmed"
	entry.ConfirmedAt = &confirmed
	c.state.Deliveries[key] = entry
	if err := c.saveState(); err != nil {
		entry.Status = "pending"
		entry.ConfirmedAt = nil
		c.state.Deliveries[key] = entry
		return false, errors.New("confirmed delivery could not be saved; not counting it as ready")
	}
	return budgetReady(budget, now), nil
}

func deliveryNeeded(entry deliveryState, exists bool, target RemoteTarget, budget budget) (need, verify bool) {
	if !exists {
		return true, false
	}
	if entry.Target != target.key() {
		return true, false
	}
	if entry.RunStartedAt != budget.runKey() || entry.Deadline != budget.deadlineKey() {
		return true, false
	}
	if entry.Status == "confirmed" {
		return false, false
	}
	return true, true
}

func sameDeadline(got, want string) bool {
	return strings.TrimRight(got, "\n") == strings.TrimRight(want, "\n")
}

// maybeWake wakes at most one standby, and only when there is no safe
// budget-ready machine or the single ready machine is inside the 105-minute
// threshold. A GET failure anywhere cancels waking for the round.
func (c *Controller) maybeWake(ctx context.Context, outcomes []machineOutcome, allGETsOK bool, now time.Time) error {
	if !c.Config.EnableWake || c.DryRun {
		return nil
	}
	if !allGETsOK || c.roundGetFailed {
		c.logf("wake skipped: a machine GET failed this round")
		return nil
	}
	var ready []machineOutcome
	for _, outcome := range outcomes {
		if outcome.ready {
			ready = append(ready, outcome)
		}
	}
	switch {
	case len(ready) > 1:
		return nil
	case len(ready) == 1 && ready[0].remaining > standbyWakeThreshold:
		return nil
	}

	for _, outcome := range outcomes {
		if outcome.ready || !outcome.hasResult {
			continue
		}
		cycle, ok := safeWakeCycle(outcome.machine, now)
		if !ok {
			continue
		}
		if attempt, exists := c.state.WakeAttempts[cycle]; exists {
			c.logf("machine %s: wake already attempted for this suspend cycle (%s), not resending", outcome.config.MachineID, attempt.Status)
			continue
		}
		return c.attemptWake(ctx, outcome.config.MachineID, cycle, now)
	}
	return nil
}

// safeWakeCycle identifies an automatically idle-paused machine. Manual
// suspend/stop, cooldowns, active operations, host trouble and any state the
// controller cannot distinguish cause a skip.
func safeWakeCycle(machine Machine, now time.Time) (string, bool) {
	if machine.State != "suspended" || machine.DesiredState != "suspended" {
		return "", false
	}
	if machine.OperationID != nil && *machine.OperationID != "" {
		return "", false
	}
	if machine.Runtime.HostUnreachable {
		return "", false
	}
	if machine.CooldownReason != nil && *machine.CooldownReason != "" {
		return "", false
	}
	if machine.CooldownUntil != nil && strings.TrimSpace(*machine.CooldownUntil) != "" {
		until, err := parseAPITime(*machine.CooldownUntil)
		if err != nil || until.After(now) {
			return "", false
		}
	}
	if machine.AutoSuspendedAt == nil || strings.TrimSpace(*machine.AutoSuspendedAt) == "" {
		return "", false
	}
	suspendedAt, err := parseAPITime(*machine.AutoSuspendedAt)
	if err != nil || suspendedAt.After(now.Add(time.Minute)) {
		return "", false
	}
	return machine.ID + "|" + suspendedAt.UTC().Format(time.RFC3339Nano), true
}

func (c *Controller) attemptWake(ctx context.Context, machineID, cycle string, now time.Time) error {
	api, err := c.apiFor(machineID)
	if err != nil {
		return fmt.Errorf("wake is enabled but %w", err)
	}
	attempt := wakeAttempt{AttemptedAt: now, Status: "pending"}
	c.state.WakeAttempts[cycle] = attempt
	// Persist before POST: a crash or restart must not turn into a second wake.
	if err := c.saveState(); err != nil {
		delete(c.state.WakeAttempts, cycle)
		return fmt.Errorf("wake attempt record could not be saved; not sending: %w", err)
	}
	c.logf("machine %s: waking automatically idle-suspended standby", machineID)
	_, err = api.WakeMachine(ctx, machineID)
	if err == nil {
		attempt.Status = "sent"
		c.state.WakeAttempts[cycle] = attempt
		_ = c.saveState()
		return nil
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		// A definite answer (including 409 operation_conflict or cooldown) is
		// final for this suspend cycle.
		attempt.Status = "failed"
		c.state.WakeAttempts[cycle] = attempt
		_ = c.saveState()
		return fmt.Errorf("%s: wake was refused for this suspend cycle: %w", machineID, err)
	}
	attempt.Status = "unknown"
	c.state.WakeAttempts[cycle] = attempt
	_ = c.saveState()

	// A timeout may have woken the machine. Disambiguate with GET using the
	// same account's client, but never resend for the same cycle.
	fresh, getErr := api.GetMachine(ctx, machineID)
	if getErr != nil {
		c.roundGetFailed = true
	}
	if getErr == nil {
		if fresh.State != "suspended" {
			attempt.Status = "resolved"
		} else if !cycleMatches(cycle, machineID, fresh.AutoSuspendedAt) {
			attempt.Status = "resolved"
		}
	}
	c.state.WakeAttempts[cycle] = attempt
	_ = c.saveState()
	return fmt.Errorf("%s: wake result unknown; not resent for this suspend cycle", machineID)
}

func cycleMatches(cycle, machineID string, suspendedAt *string) bool {
	if suspendedAt == nil {
		return false
	}
	at, err := parseAPITime(*suspendedAt)
	if err != nil {
		return false
	}
	return cycle == machineID+"|"+at.UTC().Format(time.RFC3339Nano)
}

func (c *Controller) saveState() error {
	c.state.Version = stateVersion
	if c.state.Deliveries == nil {
		c.state.Deliveries = map[string]deliveryState{}
	}
	if c.state.WakeAttempts == nil {
		c.state.WakeAttempts = map[string]wakeAttempt{}
	}
	data, err := json.MarshalIndent(c.state, "", "  ")
	if err != nil {
		return errors.New("controller state could not be encoded")
	}
	dir := c.Config.StateDir
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return errors.New("controller state could not be written")
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return errors.New("controller state permissions could not be set")
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return errors.New("controller state could not be written")
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return errors.New("controller state could not be synced")
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return errors.New("controller state could not be closed")
	}
	if err := os.Rename(tmpName, filepath.Join(dir, "state.json")); err != nil {
		os.Remove(tmpName)
		return errors.New("controller state could not be replaced")
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		dirHandle.Close()
	}
	return nil
}

func loadState(path string) (controllerState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return controllerState{}, errors.New("controller state could not be read")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	// Pointer-to-map distinguishes an absent or null required deduplication
	// field (fail closed) from a legitimate empty object.
	var fileState struct {
		Version      int                       `json:"version"`
		Deliveries   *map[string]deliveryState `json:"deliveries"`
		WakeAttempts *map[string]wakeAttempt   `json:"wakeAttempts"`
	}
	if err := dec.Decode(&fileState); err != nil {
		return controllerState{}, errors.New("controller state is corrupt; refusing to act")
	}
	if fileState.Version != stateVersion {
		return controllerState{}, errors.New("controller state has an unsupported version; refusing to act")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return controllerState{}, errors.New("controller state has trailing data; refusing to act")
	}
	if fileState.Deliveries == nil || fileState.WakeAttempts == nil {
		return controllerState{}, errors.New("controller state is missing required deduplication state; refusing to act")
	}
	return controllerState{
		Version:      fileState.Version,
		Deliveries:   *fileState.Deliveries,
		WakeAttempts: *fileState.WakeAttempts,
	}, nil
}

type lockHandle struct {
	file *os.File
}

// acquireLock prevents two controllers from writing or waking concurrently.
func acquireLock(path string) (*lockHandle, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, errors.New("controller lock file could not be opened")
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("another instabox-controller instance holds the state lock")
	}
	return &lockHandle{file: file}, nil
}

func (l *lockHandle) release() {
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}

// RemoteTarget is one validated remote deadline location.
type RemoteTarget struct {
	Host         string
	User         string
	Port         int
	IdentityFile string
	Path         string
}

func remoteTarget(m MachineConfig) RemoteTarget {
	return RemoteTarget{
		Host:         m.SSHHost,
		User:         m.SSHUser,
		Port:         m.SSHPort,
		IdentityFile: m.SSHIdentityFile,
		Path:         m.DeadlinePath,
	}
}

func (t RemoteTarget) key() string {
	return strings.Join([]string{
		t.Host,
		t.User,
		strconv.Itoa(t.Port),
		t.IdentityFile,
		t.Path,
	}, "|")
}

// validateRemoteTarget re-checks parameters at use time, so a manually built
// RemoteTarget cannot smuggle options or shell metacharacters.
func validateRemoteTarget(t RemoteTarget) error {
	if !sshHostPattern.MatchString(t.Host) {
		return errors.New("refusing SSH: unsafe host")
	}
	if !sshUserPattern.MatchString(t.User) {
		return errors.New("refusing SSH: unsafe user")
	}
	if t.Port < 1 || t.Port > 65535 {
		return errors.New("refusing SSH: unsafe port")
	}
	if t.IdentityFile != "" && !validLocalPath(t.IdentityFile) {
		return errors.New("refusing SSH: unsafe identity file")
	}
	if !validLocalPath(t.Path) {
		return errors.New("refusing SSH: unsafe deadline path")
	}
	return nil
}

func sshArgs(target RemoteTarget, script string) ([]string, error) {
	if err := validateRemoteTarget(target); err != nil {
		return nil, err
	}
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=2",
		"-o", "LogLevel=ERROR",
		"-p", strconv.Itoa(target.Port),
		"-l", target.User,
	}
	if target.IdentityFile != "" {
		args = append(args, "-i", target.IdentityFile)
	}
	return append(args, target.Host, script), nil
}

// DeadlineWriter is the narrow remote surface. ReadDeadline is used only to
// confirm an earlier unknown write before deciding to replace it.
type DeadlineWriter interface {
	ReadDeadline(ctx context.Context, target RemoteTarget) (string, error)
	WriteDeadline(ctx context.Context, target RemoteTarget, content string) (string, error)
}

// SSHWriter runs one batch-mode SSH command with a bounded timeout. Host key
// checking is left at the user's configuration; it is never disabled.
type SSHWriter struct {
	Timeout time.Duration
}

// NewSSHWriter returns a writer using the local ssh client and its known_hosts.
func NewSSHWriter() *SSHWriter {
	return &SSHWriter{Timeout: 25 * time.Second}
}

func (w *SSHWriter) ReadDeadline(ctx context.Context, target RemoteTarget) (string, error) {
	return w.run(ctx, target, readScript(target))
}

func (w *SSHWriter) WriteDeadline(ctx context.Context, target RemoteTarget, content string) (string, error) {
	script, err := writeScript(target, content)
	if err != nil {
		return "", err
	}
	return w.run(ctx, target, script)
}

func readScript(target RemoteTarget) string {
	return "cat -- " + shellQuote(target.Path)
}

func writeScript(target RemoteTarget, content string) (string, error) {
	if err := validateRemoteTarget(target); err != nil {
		return "", err
	}
	if !validLocalPath(target.Path) || strings.HasSuffix(target.Path, "/") {
		return "", errors.New("refusing SSH: unsafe deadline path")
	}
	// Create an exclusive random file in the target's own directory. A fixed
	// temp name could already be a symlink, and the shell redirect would
	// truncate whatever it pointed at. The trap removes only the file mktemp
	// just created, never an unrelated path.
	tmpTemplate := filepath.Join(filepath.Dir(target.Path), ".instabox-controller.XXXXXX")
	encoded := shellQuote(base64.StdEncoding.EncodeToString([]byte(content)))
	targetPath := shellQuote(target.Path)
	return "umask 077; tmp=$(mktemp " + shellQuote(tmpTemplate) + ") || exit 1; " +
		"trap 'rm -f -- \"$tmp\"' 0 HUP INT TERM; " +
		"printf %s " + encoded + " | base64 -d > \"$tmp\" && " +
		"mv -f -- \"$tmp\" " + targetPath + " && " +
		"cat -- " + targetPath, nil
}

func (w *SSHWriter) run(ctx context.Context, target RemoteTarget, script string) (string, error) {
	args, err := sshArgs(target, script)
	if err != nil {
		return "", err
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 25 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	// SSH stderr is discarded deliberately: it can contain paths and remote
	// noise, and the controller reports only its own bounded diagnostics.
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", errors.New("ssh command timed out")
		}
		return "", errors.New("ssh command failed")
	}
	return stdout.String(), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
