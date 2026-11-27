package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/buildinfo"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
)

const defaultConfigPath = "/etc/gha-runner-tui/config.yaml"
const defaultUnitDir = "/etc/systemd/system"

func defaultStdin() io.Reader { return os.Stdin }

func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var flagArgs, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			positionals = append(positionals, arg)
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
		name, _, hasValue := strings.Cut(name, "=")
		f := fs.Lookup(name)
		if f == nil {
			return fmt.Errorf("unknown flag: %s", name)
		}
		flagArgs = append(flagArgs, arg)
		if hasValue {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 == len(args) {
			return fmt.Errorf("flag needs a value: %s", name)
		}
		i++
		flagArgs = append(flagArgs, args[i])
	}
	return fs.Parse(append(flagArgs, append([]string{"--"}, positionals...)...))
}

func runCLI(ctx context.Context, args []string, stdout, stderr io.Writer, makeManager func(string, string) app.RunnerManager) int {
	return runCLIWithInput(ctx, args, defaultStdin(), stdout, stderr, makeManager)
}

func runCLIWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, makeManager func(string, string) app.RunnerManager) int {
	return runCommandWithInput(ctx, args, stdin, stdout, stderr, makeManager)
}

func runCommandWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, makeManager func(string, string) app.RunnerManager) int {
	if len(args) == 0 {
		return writeArgumentError(stderr, errors.New("command is required"))
	}
	cmd := args[0]
	if cmd == "sync" {
		opts, err := parseSyncArgs(args[1:])
		if err != nil {
			return writeArgumentError(stderr, fmt.Errorf("sync failed: %w", err))
		}
		// SyncProfilePath migrates before loading global config. Check it here
		// so a malformed global config cannot cause a partial CLI write.
		if _, err := config.LoadGlobalConfig(opts.configPath); err != nil {
			return writeOperationError(stderr, err)
		}
		if err := runSyncWith(ctx, opts, makeManager(opts.configPath, defaultUnitDir)); err != nil {
			return writeOperationError(stderr, fmt.Errorf("sync failed: %w", err))
		}
		return writeCLIText(stdout, stderr, "runner groups synced\n")
	}
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", defaultConfigPath, "Path to global config file")
	var jsonOutput, dockerLogs, force bool
	var noGitHub, dryRun, prune, noStart bool
	var owner, match, expectPlan, bundleFile string
	tail := 200
	input := app.CreateProfileInput{Scope: config.TargetScopeRepository, Ephemeral: true}
	var scope, labels, watch string
	admission := state.DefaultAdmissionPolicy()
	wantArgs := 0
	switch cmd {
	case "status":
		fs.BoolVar(&jsonOutput, "json", false, "Output JSON")
		fs.BoolVar(&noGitHub, "no-github", false, "Do not call GitHub")
	case "logs":
		wantArgs = 1
		fs.BoolVar(&dockerLogs, "docker", false, "Read container logs")
		fs.IntVar(&tail, "n", 200, "Number of lines")
	case "start", "restart":
		wantArgs = 1
		fs.BoolVar(&jsonOutput, "json", false, "Output JSON")
		fs.StringVar(&match, "match", "", "Profile name glob")
	case "stop":
		wantArgs = 1
		fs.BoolVar(&jsonOutput, "json", false, "Output JSON")
		fs.StringVar(&match, "match", "", "Profile name glob")
		fs.BoolVar(&force, "force", false, "Remove slot holders; running jobs will fail")
	case "mode":
		fs.IntVar(&admission.DelaySeconds, "delay-seconds", 60, "Fallback grace period (mode 2 only)")
		fs.BoolVar(&jsonOutput, "json", false, "Output JSON")
		fs.StringVar(&match, "match", "", "Profile name glob")
	case "create":
		fs.StringVar(&scope, "scope", "repository", "repository or organization")
		fs.StringVar(&input.RepoOwner, "owner", "", "Repository owner")
		fs.StringVar(&input.RepoName, "repo", "", "Repository name")
		fs.StringVar(&input.Org, "org", "", "Organization")
		fs.StringVar(&input.Environment, "environment", "", "Organization runner environment")
		fs.StringVar(&input.Name, "name", "", "Profile name")
		fs.StringVar(&labels, "labels", "", "Comma-separated runner labels")
		fs.StringVar(&input.DockerImage, "image", "", "Runner image")
		fs.StringVar(&input.CPUs, "cpus", "", "CPU limit")
		fs.StringVar(&input.Memory, "memory", "", "Memory limit")
		fs.StringVar(&input.DockerAccess, "docker-access", "", "Docker access mode")
		fs.StringVar(&input.ServiceName, "service", "", "Service unit name")
		fs.StringVar(&input.ContainerNamePrefix, "container-prefix", "", "Container name prefix")
		fs.StringVar(&input.GitHubEnvFile, "github-env-file", "", "GitHub environment file path")
		fs.StringVar(&watch, "watch", "", "Comma-separated owner/repo watch list")
		fs.BoolVar(&input.NoStart, "no-start", false, "Do not start the new unit")
	case "export":
		fs.StringVar(&owner, "owner", "all", "local, fleet, or all")
		fs.BoolVar(&jsonOutput, "json", false, "Output JSON")
	case "adopt":
		fs.StringVar(&owner, "owner", "", "local or fleet")
		fs.StringVar(&match, "match", "", "Profile name glob")
		fs.BoolVar(&dryRun, "dry-run", false, "Only print the plan")
		fs.StringVar(&expectPlan, "expect-plan", "", "Approve exactly this plan_id")
		fs.BoolVar(&jsonOutput, "json", false, "Output JSON")
	case "delete":
		fs.StringVar(&owner, "owner", "local", "local or fleet")
		fs.BoolVar(&force, "force", false, "Also remove running slot holders")
		fs.BoolVar(&jsonOutput, "json", false, "Output JSON")
	case "apply":
		fs.StringVar(&bundleFile, "f", "", "Bundle file, or - for stdin")
		fs.StringVar(&owner, "owner", "local", "local or fleet")
		fs.BoolVar(&prune, "prune", false, "Delete fleet profiles absent from the bundle")
		fs.BoolVar(&dryRun, "dry-run", false, "Only print the plan")
		fs.StringVar(&expectPlan, "expect-plan", "", "Approve exactly this plan_id")
		fs.BoolVar(&noStart, "no-start", false, "Create profiles without starting units")
		fs.BoolVar(&jsonOutput, "json", false, "Output JSON")
	case "migrate", "version":
	default:
		return writeArgumentError(stderr, fmt.Errorf("unknown command: %s", cmd))
	}
	if err := parseInterspersed(fs, args[1:]); err != nil {
		return writeArgumentError(stderr, err)
	}
	if cmd == "mode" {
		maxArgs := 2
		if match != "" {
			maxArgs = 1
		}
		if fs.NArg() > maxArgs {
			return writeArgumentError(stderr, errors.New("usage: mode PROFILE [0|1|2] [--delay-seconds N] [--match GLOB]"))
		}
		delaySet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "delay-seconds" {
				delaySet = true
			}
		})
		modeArg := -1
		if fs.NArg() == 2 {
			modeArg = 1
		} else if fs.NArg() == 1 && match != "" {
			modeArg = 0
		}
		if modeArg >= 0 {
			mode, err := strconv.Atoi(fs.Arg(modeArg))
			if err != nil {
				return writeArgumentError(stderr, errors.New("mode must be 0, 1, or 2"))
			}
			admission.Mode = state.AdmissionMode(mode)
			if err := admission.Validate(); err != nil {
				return writeArgumentError(stderr, err)
			}
		}
		if delaySet && (modeArg == -1 || admission.Mode != state.AdmissionDelayed) {
			return writeArgumentError(stderr, errors.New("--delay-seconds requires mode 2"))
		}
	} else if cmd == "export" || cmd == "adopt" {
		// Names are optional; --match selects adopt targets.
	} else if cmd == "apply" {
		if fs.NArg() != 0 {
			return writeArgumentError(stderr, errors.New("apply takes no positional arguments; use -f"))
		}
		if bundleFile != "" && (bundleFile == "-" && stdin == nil) {
			return writeArgumentError(stderr, errors.New("apply -f - requires stdin"))
		}
	} else if cmd == "delete" {
		if fs.NArg() != 1 {
			return writeArgumentError(stderr, errors.New("delete requires 1 profile name"))
		}
	} else if fs.NArg() != wantArgs {
		if match == "" || (cmd != "start" && cmd != "stop" && cmd != "restart") || fs.NArg() != 0 {
			return writeArgumentError(stderr, fmt.Errorf("%s requires %d positional argument(s)", cmd, wantArgs))
		}
	}
	if cmd == "mode" {
		if match == "" && fs.NArg() == 0 {
			return writeArgumentError(stderr, errors.New("usage: mode PROFILE [0|1|2] [--delay-seconds N]"))
		}
	} else if cmd == "start" || cmd == "stop" || cmd == "restart" {
		if match == "" && fs.NArg() == 0 {
			return writeArgumentError(stderr, fmt.Errorf("%s requires %d positional argument(s)", cmd, wantArgs))
		}
		if match != "" && fs.NArg() > 0 {
			return writeArgumentError(stderr, errors.New("use a profile name or --match, not both"))
		}
	}
	if cmd == "stop" && match != "" && force {
		return writeArgumentError(stderr, errors.New("stop --match does not allow --force"))
	}
	if cmd == "delete" && strings.TrimSpace(fs.Arg(0)) == "" {
		return writeArgumentError(stderr, errors.New("profile name is required"))
	}
	if (wantArgs == 1 || cmd == "mode") && match == "" && fs.NArg() > 0 && strings.TrimSpace(fs.Arg(0)) == "" {
		return writeArgumentError(stderr, errors.New("profile name is required"))
	}
	if tail < 0 {
		return writeArgumentError(stderr, errors.New("logs -n must not be negative"))
	}
	if cmd == "apply" && bundleFile == "" {
		return writeArgumentError(stderr, errors.New("apply requires -f PATH|-"))
	}
	if cmd == "adopt" {
		if owner != config.ManagedByLocal && owner != config.ManagedByFleet {
			return writeArgumentError(stderr, errors.New("adopt requires --owner local|fleet"))
		}
	} else if cmd == "export" {
		if owner != "all" && owner != config.ManagedByLocal && owner != config.ManagedByFleet {
			return writeArgumentError(stderr, errors.New("--owner must be local, fleet, or all"))
		}
	} else if cmd == "apply" || cmd == "delete" {
		if owner != config.ManagedByLocal && owner != config.ManagedByFleet {
			return writeArgumentError(stderr, errors.New("--owner must be local or fleet"))
		}
	}
	if cmd == "create" {
		input.Scope = config.TargetScope(scope)
		input.RunnerLabels, input.WatchRepositories = splitCSV(labels), splitCSV(watch)
		if err := validateCLICreate(&input); err != nil {
			return writeOperationError(stderr, err)
		}
	}
	if cmd == "version" {
		return writeCLIText(stdout, stderr, buildinfo.Current())
	}
	if _, err := config.LoadGlobalConfig(*configPath); err != nil {
		return writeOperationError(stderr, err)
	}
	m := makeManager(*configPath, defaultUnitDir)
	switch cmd {
	case "status":
		m.Service.NoGitHub = noGitHub
		d, err := m.Dashboard(ctx)
		if err != nil {
			return writeOperationError(stderr, err)
		}
		if err := writeStatus(stdout, d, jsonOutput); err != nil {
			return writeOperationError(stderr, err)
		}
		return 0
	case "create":
		if err := m.CreateProfile(ctx, input); err != nil {
			return writeOperationError(stderr, err)
		}
		return writeCLIText(stdout, stderr, fmt.Sprintf("created profile %s\n", input.Name))
	case "migrate":
		results, err := m.Migrate(ctx)
		for _, result := range results {
			if code := writeCLIText(stdout, stderr, fmt.Sprintf("%s\t%s\t%s\n", result.Path, result.Status, result.Message)); code != 0 {
				return code
			}
		}
		if err != nil {
			return writeOperationError(stderr, err)
		}
		return 0
	case "logs":
		p, err := m.LookupProfile(fs.Arg(0))
		if err != nil {
			return writeOperationError(stderr, err)
		}
		snapshot := app.ProfileSnapshot{Profile: p}
		var text string
		if dockerLogs {
			snapshot.Loop, _ = state.LoadLoopState(p.Loop.StateFile)
			text, err = m.DockerLogs(ctx, snapshot, tail, false)
		} else {
			text, err = m.SystemdLogs(ctx, p, tail, false)
		}
		if err != nil {
			return writeOperationError(stderr, err)
		}
		return writeCLIText(stdout, stderr, text)
	case "export":
		report, err := m.Export(ctx, app.ExportInput{Names: fs.Args(), Owner: owner})
		if report.Profiles == nil {
			report.Profiles = []app.ExportEntry{}
		}
		if report.Errors == nil {
			report.Errors = []app.ExportError{}
		}
		if jsonOutput {
			if code := writeFleetJSON(stdout, stderr, report); code != 0 {
				return code
			}
			if err != nil {
				fmt.Fprintln(stderr, err)
				if errors.Is(err, app.ErrFleetInput) {
					return 2
				}
				return 1
			}
			if !report.Complete {
				return 1
			}
			return 0
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			if errors.Is(err, app.ErrFleetInput) {
				return 2
			}
			return 1
		}
		for _, p := range report.Profiles {
			fmt.Fprintf(stdout, "%s\t%s\t%s\n", p.Name, p.Owner, p.Path)
		}
		for _, e := range report.Errors {
			fmt.Fprintf(stderr, "%s: %s\n", e.Path, e.Error)
		}
		if !report.Complete {
			return 1
		}
		return 0
	case "adopt":
		report, err := m.Adopt(ctx, app.AdoptInput{Names: fs.Args(), Match: match, Owner: owner, DryRun: dryRun, ExpectPlan: expectPlan})
		return finishFleetReport(ctx, stdout, stderr, report, err, jsonOutput, func() error {
			for _, r := range report.Results {
				fmt.Fprintf(stdout, "%s\t%s\t%s\n", r.Name, r.Action, r.Message)
			}
			return nil
		})
	case "apply":
		bundle, readErr := readApplyBundle(stdin, bundleFile)
		if readErr != nil {
			return writeArgumentError(stderr, readErr)
		}
		report, err := m.Apply(ctx, app.ApplyInput{Bundle: bundle, Owner: owner, Prune: prune, DryRun: dryRun, ExpectPlan: expectPlan, NoStart: noStart})
		return finishFleetReport(ctx, stdout, stderr, report, err, jsonOutput, func() error {
			fmt.Fprintf(stdout, "phase=%s plan_id=%s complete=%t\n", report.Phase, report.PlanID, report.Complete)
			for _, r := range report.Results {
				fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", r.Name, r.Action, r.Restart, r.Outcome, r.Message)
			}
			return nil
		})
	case "delete":
		report, err := m.Delete(ctx, app.DeleteInput{Name: fs.Arg(0), Owner: owner, Force: force})
		return finishFleetReport(ctx, stdout, stderr, report, err, jsonOutput, func() error {
			fmt.Fprintf(stdout, "phase=%s plan_id=%s complete=%t\n", report.Phase, report.PlanID, report.Complete)
			for _, r := range report.Results {
				fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", r.Name, r.Action, r.Outcome, r.Message)
			}
			return nil
		})
	}
	return runRuntimeCLI(ctx, m, cmd, match, jsonOutput, force, admission, fs, stdout, stderr)
}

func splitCSV(value string) []string {
	items := []string{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// Validate CLI-only requirements before constructing a manager. Shared create
// validation and exclusive file creation remain in the app layer.
func validateCLICreate(input *app.CreateProfileInput) error {
	invalid := func(message string) error { return fmt.Errorf("%w: %s", app.ErrInvalidCreateInput, message) }
	values := []string{string(input.Scope), input.Org, input.Environment, input.DockerAccess, input.Name, input.RepoOwner, input.RepoName, input.DockerImage, input.ServiceName, input.ContainerNamePrefix, input.CPUs, input.Memory, input.GitHubEnvFile}
	values = append(values, input.RunnerLabels...)
	values = append(values, input.WatchRepositories...)
	for _, value := range values {
		if strings.ContainsAny(value, "\r\n\x00") {
			return invalid("create values must not contain CR, LF, or NUL")
		}
	}
	if len(input.RunnerLabels) == 0 || strings.TrimSpace(input.DockerImage) == "" || strings.TrimSpace(input.CPUs) == "" || strings.TrimSpace(input.Memory) == "" {
		return invalid("labels, image, cpus, and memory are required")
	}
	switch strings.TrimSpace(input.DockerAccess) {
	case "", "default", "rootless", "host-socket":
	default:
		return invalid("unsupported docker access mode")
	}
	groupName := ""
	switch input.Scope {
	case config.TargetScopeRepository:
		input.RepoOwner, input.RepoName = strings.TrimSpace(input.RepoOwner), strings.TrimSpace(input.RepoName)
		if input.RepoOwner == "" || input.RepoName == "" || strings.Contains(input.RepoOwner+input.RepoName, "/") {
			return invalid("owner and repo are required repository components")
		}
		if len(input.WatchRepositories) > 1 || (len(input.WatchRepositories) == 1 && !strings.EqualFold(input.WatchRepositories[0], input.RepoOwner+"/"+input.RepoName)) {
			return invalid("repository watch must contain only its own owner/repo")
		}
	case config.TargetScopeOrganization:
		names, err := config.DeriveOrganizationEnvironmentNames(input.Org, input.Environment, "", "")
		if err != nil {
			return invalid(err.Error())
		}
		if input.Name == "" {
			input.Name = names.ProfileName
		}
		groupName = names.RunnerGroupName
		if len(input.WatchRepositories) == 0 {
			return invalid("organization watch repositories are required")
		}
	default:
		return invalid("scope must be repository or organization")
	}
	for _, entry := range input.WatchRepositories {
		owner, repo, ok := strings.Cut(entry, "/")
		if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
			return invalid("watch entries must be owner/repo")
		}
	}
	if input.ServiceName == "" {
		input.ServiceName = "gha-" + input.Name + ".service"
	}
	if input.ContainerNamePrefix == "" {
		input.ContainerNamePrefix = "gha-" + input.Name
	}
	p := config.Profile{Name: input.Name, Target: config.TargetConfig{Scope: input.Scope, Org: input.Org},
		Repo: config.RepoConfig{Owner: input.RepoOwner, Name: input.RepoName}, Service: config.ServiceConfig{Name: input.ServiceName},
		Docker: config.DockerProfile{ContainerNamePrefix: input.ContainerNamePrefix}, Runner: config.RunnerConfig{Environment: input.Environment}, RunnerGroup: config.RunnerGroupConfig{Name: groupName}}
	if err := p.Validate(); err != nil {
		return invalid(err.Error())
	}
	return nil
}

func writeArgumentError(stderr io.Writer, err error) int {
	if _, err := fmt.Fprintln(stderr, err); err != nil {
		return 1
	}
	return 2
}

func writeOperationError(stderr io.Writer, err error) int {
	if _, writeErr := fmt.Fprintln(stderr, err); writeErr != nil {
		return 1
	}
	if errors.Is(err, app.ErrInvalidCreateInput) {
		return 2
	}
	return 1
}

func writeCLIText(stdout, stderr io.Writer, text string) int {
	n, err := io.WriteString(stdout, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return writeOperationError(stderr, err)
	}
	return 0
}

func writeStatus(w io.Writer, d app.Dashboard, jsonOutput bool) error {
	if jsonOutput {
		type slotDTO struct {
			Profile   string                `json:"profile"`
			Container string                `json:"container"`
			Runner    string                `json:"runner"`
			State     state.ContainerStatus `json:"state"`
		}
		type profileDTO struct {
			Name      string                 `json:"name"`
			Admission *state.AdmissionPolicy `json:"admission,omitempty"`
			Service   struct {
				Active  state.SystemdStatus `json:"active"`
				Enabled bool                `json:"enabled"`
			} `json:"service"`
			Loop struct {
				State            state.LoopStatus `json:"state"`
				LastTransitionAt string           `json:"last_transition_at"`
				LastError        *string          `json:"last_error"`
			} `json:"loop"`
			Container struct {
				Name  string                `json:"name"`
				State state.ContainerStatus `json:"state"`
			} `json:"container"`
			GitHub struct {
				State state.GitHubStatus `json:"state"`
				Busy  state.BusyStatus   `json:"busy"`
			} `json:"github"`
			Health         state.CombinedHealth `json:"health"`
			RestartPending bool                 `json:"restart_pending"`
			Errors         []string             `json:"errors"`
		}
		type profileErrorDTO struct {
			Path  string `json:"path"`
			Error string `json:"error"`
		}
		output := struct {
			SlotHolders   []slotDTO         `json:"slot_holders"`
			SlotError     *string           `json:"slot_error"`
			Profiles      []profileDTO      `json:"profiles"`
			ProfileErrors []profileErrorDTO `json:"profile_errors"`
		}{SlotHolders: []slotDTO{}, SlotError: d.SlotError, Profiles: []profileDTO{}, ProfileErrors: []profileErrorDTO{}}
		for _, c := range d.SlotHolders {
			output.SlotHolders = append(output.SlotHolders, slotDTO{c.Profile, c.Name, c.RunnerName, c.State})
		}
		for _, p := range d.Profiles {
			item := profileDTO{Name: p.Profile.Name, Admission: p.Admission, Health: p.Health, Errors: append([]string{}, p.Errors...)}
			item.RestartPending = p.RestartPending
			item.Service.Active, item.Service.Enabled = p.Service.Active, p.Service.Enabled
			item.Loop.State, item.Loop.LastError = p.DisplayLoopState, p.Loop.LastError
			if !p.Loop.LastTransitionAt.IsZero() {
				item.Loop.LastTransitionAt = p.Loop.LastTransitionAt.UTC().Format(time.RFC3339)
			}
			item.Container.Name, item.Container.State = p.Container.Name, p.Container.State
			item.GitHub.State, item.GitHub.Busy = p.GitHubState, p.BusyState
			output.Profiles = append(output.Profiles, item)
		}
		for _, e := range d.ProfileErrors {
			output.ProfileErrors = append(output.ProfileErrors, profileErrorDTO{e.Path, e.Err.Error()})
		}
		data, err := json.Marshal(output)
		if err != nil {
			return err
		}
		data = append(data, '\n')
		n, err := w.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		return err
	}
	var text strings.Builder
	switch {
	case d.SlotError != nil:
		fmt.Fprintf(&text, "SLOT: unknown (%s)\n", *d.SlotError)
	case len(d.SlotHolders) == 0:
		text.WriteString("SLOT: free\n")
	case len(d.SlotHolders) > 1:
		fmt.Fprintf(&text, "SLOT: %d occupied\n", len(d.SlotHolders))
	}
	for _, c := range d.SlotHolders {
		prefix := "  "
		if len(d.SlotHolders) == 1 && d.SlotError == nil {
			prefix = "SLOT: "
		}
		fmt.Fprintf(&text, "%s%s %s (container %s, runner %s)\n", prefix, c.Profile, c.State, c.Name, c.RunnerName)
	}
	table := tabwriter.NewWriter(&text, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "PROFILE\tMODE\tSERVICE\tLOOP\tCONTAINER\tGITHUB\tBUSY\tHEALTH")
	for _, p := range d.Profiles {
		enabled := "disabled"
		if p.Service.Enabled {
			enabled = "enabled"
		}
		mode := "unknown"
		if p.Admission != nil {
			mode = strconv.Itoa(int(p.Admission.Mode))
			if p.Admission.Mode == state.AdmissionDelayed {
				mode += fmt.Sprintf("/%ds", p.Admission.DelaySeconds)
			}
		}
		fmt.Fprintf(table, "%s\t%s\t%s/%s\t%s\t%s\t%s\t%s\t%s\n", p.Profile.Name, mode, p.Service.Active, enabled, p.DisplayLoopState, p.Container.State, p.GitHubState, p.BusyState, p.Health)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	for _, p := range d.Profiles {
		for _, err := range p.Errors {
			fmt.Fprintf(&text, "%s: %s\n", p.Profile.Name, err)
		}
	}
	for _, err := range d.ProfileErrors {
		fmt.Fprintln(&text, err.Error())
	}
	n, err := io.WriteString(w, text.String())
	if err == nil && n != text.Len() {
		err = io.ErrShortWrite
	}
	return err
}
