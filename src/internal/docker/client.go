package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gha-runner-tui/internal/command"
	"gha-runner-tui/internal/state"
)

const dockerHost = "unix:///var/run/docker.sock"

const managedLabelFilter = "label=io.gha-runner-tui.managed=true"

// DiskReserveLabel marks a running runner's profile-declared Docker data-root
// reservation. The value is a decimal byte count; the label is absent when the
// profile reserves nothing.
const DiskReserveLabel = "io.gha-runner-tui.disk-reserve"

const diskReserveListFormat = `{{.ID}}\t{{.Label "io.gha-runner-tui.disk-reserve"}}`

const managedListFormat = `{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Label "io.gha-runner-tui.profile"}}\t{{.Label "io.gha-runner-tui.runner"}}`

var ErrContainerNotFound = errors.New("docker container not found")

var (
	errInspectUnavailable = errors.New("docker inspect failed")
	errInspectMalformed   = errors.New("docker inspect returned invalid container data")
)

type Client struct {
	runner command.Runner
}

type ContainerInfo struct {
	ID         string
	Name       string
	Image      string
	StatusText string
	State      state.ContainerStatus
	Profile    string
	RunnerName string
}

type ContainerDetails struct {
	ID        string
	Name      string
	Image     string
	State     state.ContainerStatus
	Env       map[string]string
	Labels    map[string]string
	CreatedAt time.Time
	StartedAt time.Time
	ExitCode  int
}

type RunSpec struct {
	Name    string
	Image   string
	CPUs    string
	Memory  string
	Volumes []string
	Env     map[string]string
	Labels  map[string]string
}

func NewClient(runner command.Runner) Client {
	return Client{runner: runner}
}

// run funnels every docker invocation through the fixed host endpoint so the
// caller's DOCKER_HOST environment cannot redirect slot bookkeeping.
func (c Client) run(ctx context.Context, args ...string) ([]byte, error) {
	return c.runner.Run(ctx, "docker", append([]string{"--host", dockerHost}, args...)...)
}

// OccupiesSlot reports whether a managed container can still occupy the
// host-wide single-job slot. Unknown states fail closed and count as occupied.
func OccupiesSlot(c ContainerInfo) bool {
	switch c.State {
	case state.ContainerExited, state.ContainerDead, state.ContainerRemoving:
		return false
	default:
		return true
	}
}

func (c Client) CurrentOrLatest(ctx context.Context, prefix string) (ContainerInfo, error) {
	containers, err := c.ListByPrefix(ctx, prefix)
	if err != nil {
		return ContainerInfo{State: state.ContainerNone}, err
	}
	if len(containers) == 0 {
		return ContainerInfo{State: state.ContainerNone}, nil
	}
	return containers[0], nil
}

func (c Client) ListByPrefix(ctx context.Context, prefix string) ([]ContainerInfo, error) {
	out, err := c.run(ctx, "ps", "--all", "--filter", "name="+prefix, "--format", "{{.ID}}\t{{.Names}}\t{{.Image}}\t{{.Status}}")
	if err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, nil
	}

	containers := make([]ContainerInfo, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		container, parseErr := ParseContainerLine(line)
		if parseErr != nil {
			return nil, parseErr
		}
		containers = append(containers, container)
	}
	return containers, nil
}

// ListManaged returns managed containers by label. An empty profile queries
// every managed container; a non-empty profile narrows by the profile label.
func (c Client) ListManaged(ctx context.Context, profile string) ([]ContainerInfo, error) {
	args := []string{"ps", "--all", "--filter", managedLabelFilter}
	if profile != "" {
		args = append(args, "--filter", "label=io.gha-runner-tui.profile="+profile)
	}
	args = append(args, "--format", managedListFormat)

	out, err := c.run(ctx, args...)
	if err != nil {
		return nil, err
	}

	// Split on newlines and skip blank rows individually: trimming the whole
	// output would drop a trailing empty label column from the final row.
	containers := make([]ContainerInfo, 0)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 6 {
			return nil, errors.New("unexpected docker ps line")
		}
		if parts[3] == "" {
			return nil, errors.New("docker ps returned empty container state")
		}
		containers = append(containers, ContainerInfo{
			ID:         parts[0],
			Name:       parts[1],
			Image:      parts[2],
			State:      state.NormalizeContainerStatus(parts[3]),
			Profile:    parts[4],
			RunnerName: parts[5],
		})
	}
	return containers, nil
}

// SlotHolders lists every managed container that currently occupies the
// host-wide single-job slot, failing closed on query errors.
func (c Client) SlotHolders(ctx context.Context) ([]ContainerInfo, error) {
	containers, err := c.ListManaged(ctx, "")
	if err != nil {
		return nil, err
	}
	holders := make([]ContainerInfo, 0, len(containers))
	for _, container := range containers {
		if OccupiesSlot(container) {
			holders = append(holders, container)
		}
	}
	return holders, nil
}

func ParseContainerLine(line string) (ContainerInfo, error) {
	parts := strings.SplitN(line, "\t", 4)
	if len(parts) != 4 {
		return ContainerInfo{}, fmt.Errorf("unexpected docker line: %q", line)
	}

	return ContainerInfo{
		ID:         parts[0],
		Name:       parts[1],
		Image:      parts[2],
		StatusText: parts[3],
		State:      normalizeDockerStatus(parts[3]),
	}, nil
}

func (c Client) Logs(ctx context.Context, container string, tail int, follow bool) (string, error) {
	args := []string{"logs", "--tail", fmt.Sprintf("%d", tail)}
	if follow {
		args = append(args, "-f")
	}
	args = append(args, container)
	return c.readLogsRedacted(ctx, container, args...)
}

func (c Client) Kill(ctx context.Context, container string) error {
	_, err := c.run(ctx, "kill", container)
	return err
}

func (c Client) CleanupExited(ctx context.Context, prefix string) ([]string, error) {
	containers, err := c.ListByPrefix(ctx, prefix)
	if err != nil {
		return nil, err
	}
	removed := make([]string, 0)
	for _, container := range containers {
		if container.State != state.ContainerExited && container.State != state.ContainerDead {
			continue
		}
		if _, err := c.run(ctx, "rm", container.ID); err != nil {
			return removed, err
		}
		removed = append(removed, container.Name)
	}
	return removed, nil
}

// ReclaimCaches prunes only rebuildable data: unused build cache, dangling
// (untagged) images, and anonymous volumes no container references. Tagged
// images, named volumes and containers are never touched. Every step runs;
// failures are joined.
func (c Client) ReclaimCaches(ctx context.Context) error {
	var errs []error
	for _, args := range [][]string{
		{"builder", "prune", "--all", "--force"},
		{"image", "prune", "--force"},
		{"volume", "prune", "--force"},
	} {
		if _, err := c.run(ctx, args...); err != nil {
			errs = append(errs, fmt.Errorf("docker %s prune: %w", args[0], err))
		}
	}
	return errors.Join(errs...)
}

// DiskReservation is one running container's declared disk reserve and its
// current writable-layer usage. Reserve is the label's decimal byte count;
// SizeRw is what that container has actually written on top of its image.
type DiskReservation struct {
	ID      string
	Reserve uint64
	SizeRw  uint64
}

// DiskReservations lists running containers carrying a disk-reserve label and
// reads their writable-layer sizes in one inspect call. It returns nil when no
// labeled container is running. Partial, malformed, or failed output is an
// error: a launch gate must never treat unknown reservations as zero.
func (c Client) DiskReservations(ctx context.Context) ([]DiskReservation, error) {
	out, err := c.run(ctx, "ps", "--filter", "label="+DiskReserveLabel, "--format", diskReserveListFormat)
	if err != nil {
		return nil, fmt.Errorf("list disk reservations: %w", err)
	}

	reservations := make([]DiskReservation, 0)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || parts[1] == "" {
			return nil, errors.New("unexpected docker disk reservation line")
		}
		reserve, parseErr := strconv.ParseUint(parts[1], 10, 64)
		if parseErr != nil {
			return nil, errors.New("invalid docker disk reservation label")
		}
		reservations = append(reservations, DiskReservation{ID: parts[0], Reserve: reserve})
	}
	if len(reservations) == 0 {
		return nil, nil
	}

	args := append([]string{"container", "inspect", "--size", "--format", "{{.SizeRw}}"}, reservationIDs(reservations)...)
	out, err = c.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("inspect disk reservation sizes: %w", err)
	}
	sizes := strings.Split(strings.TrimRight(string(out), "\r\n"), "\n")
	if len(sizes) != len(reservations) {
		return nil, errors.New("docker disk reservation size count mismatch")
	}
	for i, line := range sizes {
		size, parseErr := strconv.ParseUint(strings.TrimRight(strings.TrimSpace(line), "\r"), 10, 64)
		if parseErr != nil {
			return nil, errors.New("invalid docker disk reservation size")
		}
		reservations[i].SizeRw = size
	}
	return reservations, nil
}

func reservationIDs(reservations []DiskReservation) []string {
	ids := make([]string, len(reservations))
	for i, reservation := range reservations {
		ids[i] = reservation.ID
	}
	return ids
}

func (c Client) Inspect(ctx context.Context, idOrName string) (ContainerDetails, error) {
	out, err := c.run(ctx, "inspect", idOrName)
	if err != nil {
		// Docker 29 prints "error: no such object"; older CLIs capitalize it.
		if strings.Contains(strings.ToLower(string(out)), "no such object") {
			return ContainerDetails{}, fmt.Errorf("%w: %s", ErrContainerNotFound, idOrName)
		}
		return ContainerDetails{}, errInspectUnavailable
	}

	var payload []struct {
		ID      string `json:"Id"`
		Name    string `json:"Name"`
		Created string `json:"Created"`
		Config  struct {
			Image  string            `json:"Image"`
			Env    []string          `json:"Env"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Status    string `json:"Status"`
			StartedAt string `json:"StartedAt"`
			ExitCode  int    `json:"ExitCode"`
		} `json:"State"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return ContainerDetails{}, errInspectMalformed
	}
	if len(payload) != 1 {
		return ContainerDetails{}, errInspectMalformed
	}

	createdAt, err := time.Parse(time.RFC3339Nano, payload[0].Created)
	if err != nil {
		return ContainerDetails{}, errInspectMalformed
	}
	startedAt, err := time.Parse(time.RFC3339Nano, payload[0].State.StartedAt)
	if err != nil {
		return ContainerDetails{}, errInspectMalformed
	}

	env := map[string]string{}
	for _, entry := range payload[0].Config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			env[key] = value
		}
	}

	return ContainerDetails{
		ID:        payload[0].ID,
		Name:      strings.TrimPrefix(payload[0].Name, "/"),
		Image:     payload[0].Config.Image,
		State:     state.NormalizeContainerStatus(payload[0].State.Status),
		Env:       env,
		Labels:    payload[0].Config.Labels,
		CreatedAt: createdAt,
		StartedAt: startedAt,
		ExitCode:  payload[0].State.ExitCode,
	}, nil
}

func (c Client) RunDetached(ctx context.Context, spec RunSpec) (string, error) {
	args := []string{"run", "-d", "--sig-proxy=false", "--name", spec.Name}
	for _, key := range sortedKeys(spec.Labels) {
		args = append(args, "--label", key+"="+spec.Labels[key])
	}
	if spec.CPUs != "" {
		args = append(args, "--cpus", spec.CPUs)
	}
	if spec.Memory != "" {
		args = append(args, "--memory", spec.Memory)
	}
	for _, volume := range spec.Volumes {
		args = append(args, "-v", volume)
	}
	for _, key := range sortedKeys(spec.Env) {
		args = append(args, "-e", key+"="+spec.Env[key])
	}
	args = append(args, spec.Image)

	out, err := c.run(ctx, args...)
	if err != nil {
		// Never surface args or output: they can carry the registration token.
		return "", errors.New("docker run failed")
	}
	return strings.TrimSpace(string(out)), nil
}

func (c Client) Wait(ctx context.Context, container string) (int, error) {
	out, err := c.run(ctx, "wait", container)
	if err != nil {
		return 0, err
	}
	exitCode, parseErr := strconv.Atoi(strings.TrimSpace(string(out)))
	if parseErr != nil {
		return 0, parseErr
	}
	return exitCode, nil
}

func (c Client) Remove(ctx context.Context, container string, force bool) error {
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	args = append(args, container)
	_, err := c.run(ctx, args...)
	return err
}

func (c Client) ReadLogs(ctx context.Context, container string) (string, error) {
	return c.readLogsRedacted(ctx, container, "logs", container)
}

// logTokens narrows an inspect call to Config.Env only, so log redaction does
// not depend on full state/timestamp decoding succeeding. It never returns raw
// output or underlying command text in its error.
func (c Client) logTokens(ctx context.Context, container string) ([]string, error) {
	unavailable := errors.New("cannot obtain container log redaction data")
	out, err := c.run(ctx, "inspect", container)
	if err != nil {
		return nil, unavailable
	}

	var payload []struct {
		Config *struct {
			Env *[]string `json:"Env"`
		} `json:"Config"`
	}
	if json.Unmarshal(out, &payload) != nil || len(payload) != 1 || payload[0].Config == nil || payload[0].Config.Env == nil {
		return nil, unavailable
	}

	var tokens []string
	for _, entry := range *payload[0].Config.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, unavailable
		}
		if (key == "RUNNER_TOKEN" || key == "REG_TOKEN") && value != "" {
			tokens = append(tokens, value)
		}
	}
	sort.Slice(tokens, func(i, j int) bool { return len(tokens[i]) > len(tokens[j]) })
	return tokens, nil
}

// readLogsRedacted is the only path that reads docker logs. It refuses to emit
// logs unless the required token values were reliably decoded, then masks them.
func (c Client) readLogsRedacted(ctx context.Context, container string, args ...string) (string, error) {
	tokens, err := c.logTokens(ctx, container)
	if err != nil {
		return "", err
	}

	out, runErr := c.run(ctx, args...)
	text := string(out)
	for _, token := range tokens {
		text = strings.ReplaceAll(text, token, "[REDACTED]")
	}
	if runErr != nil {
		return text, errors.New("docker logs failed")
	}
	return text, nil
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func normalizeDockerStatus(value string) state.ContainerStatus {
	lower := strings.ToLower(value)
	switch {
	case lower == "":
		return state.ContainerNone
	case lower == "running":
		return state.ContainerRunning
	case lower == "created":
		return state.ContainerCreated
	case lower == "exited":
		return state.ContainerExited
	case lower == "dead":
		return state.ContainerDead
	case strings.HasPrefix(lower, "up "):
		return state.ContainerRunning
	case strings.HasPrefix(lower, "created"):
		return state.ContainerCreated
	case strings.HasPrefix(lower, "exited"):
		return state.ContainerExited
	case strings.HasPrefix(lower, "dead"):
		return state.ContainerDead
	default:
		return state.ContainerUnknown
	}
}
