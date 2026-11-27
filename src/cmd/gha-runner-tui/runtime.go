package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/state"
)

// runRuntimeCLI implements mode/start/stop/restart with NAME or --match.
type runtimeResult struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Message string `json:"message"`
}

func readApplyBundle(stdin io.Reader, bundleFile string) ([]byte, error) {
	if bundleFile == "-" {
		if stdin == nil {
			return nil, errors.New("apply -f - requires stdin")
		}
		return readLimited(stdin, maxBundleInputBytes)
	}
	return readBundleFile(bundleFile)
}

func runRuntimeCLI(ctx context.Context, m app.RunnerManager, cmd, pattern string, jsonOutput, force bool, admission state.AdmissionPolicy, fs *flag.FlagSet, stdout, stderr io.Writer) int {
	_ = force
	cfg, err := config.LoadGlobalConfig(m.ConfigPath)
	if err != nil {
		return writeOperationError(stderr, err)
	}
	var names []string
	modeValueSet := false
	if cmd == "mode" {
		if pattern != "" {
			modeValueSet = fs.NArg() == 1
		} else {
			modeValueSet = fs.NArg() == 2
		}
	}
	if pattern != "" {
		var matchErr error
		names, matchErr = app.MatchProfileNames(cfg.Paths.ProfilesDir, pattern)
		if matchErr != nil {
			return writeArgumentError(stderr, matchErr)
		}
		if len(names) == 0 {
			if jsonOutput {
				_ = writeJSONLine(stdout, struct {
					Results []runtimeResult `json:"results"`
				}{Results: []runtimeResult{}})
			}
			fmt.Fprintln(stderr, "no profiles matched "+pattern)
			return 1
		}
	} else {
		names = []string{fs.Arg(0)}
	}
	results := make([]runtimeResult, 0, len(names))
	code := 0
	attempted := false
	for _, name := range names {
		p, lookupErr := m.LookupProfile(name)
		if lookupErr != nil {
			results = append(results, runtimeResult{Name: name, Outcome: "failed", Message: lookupErr.Error()})
			code = 1
			continue
		}
		var runErr error
		message := ""
		attempted = true
		switch cmd {
		case "mode":
			if modeValueSet {
				if err := state.SaveAdmissionPolicy(p.Loop.StateFile, admission); err != nil {
					runErr = fmt.Errorf("set mode (run with sudo if permission is denied): %w", err)
				} else {
					message = fmt.Sprintf("profile %s: mode=%d delay=%ds (0=paused, 1=normal, 2=delayed)", p.Name, admission.Mode, admission.DelaySeconds)
				}
			} else {
				loaded, err := state.LoadAdmissionPolicy(p.Loop.StateFile)
				if err != nil {
					runErr = err
				} else {
					message = fmt.Sprintf("profile %s: mode=%d delay=%ds (0=paused, 1=normal, 2=delayed)", p.Name, loaded.Mode, loaded.DelaySeconds)
				}
			}
		case "start":
			runErr = m.StartLoop(ctx, p)
			message = fmt.Sprintf("start profile %s (unit %s)", p.Name, p.Service.Name)
		case "restart":
			runErr = m.RestartLoop(ctx, app.ProfileSnapshot{Profile: p})
			message = fmt.Sprintf("restart profile %s (unit %s)", p.Name, p.Service.Name)
		case "stop":
			runErr = m.StopLoop(ctx, app.ProfileSnapshot{Profile: p})
			message = fmt.Sprintf("stop profile %s (unit %s)", p.Name, p.Service.Name)
			if runErr == nil && pattern == "" {
				holders, err := m.ProfileSlotHolders(ctx, p)
				if err != nil {
					runErr = err
					break
				}
				if force {
					if _, err := fmt.Fprintln(stderr, "WARNING: 正在跑的 job 会失败"); err != nil {
						return 1
					}
					removed, err := m.ForceRemoveProfile(ctx, p)
					if err != nil {
						runErr = err
						break
					}
					for _, container := range removed {
						if code := writeCLIText(stdout, stderr, "removed container "+container+"\n"); code != 0 {
							return code
						}
					}
				} else {
					for _, holder := range holders {
						text := fmt.Sprintf("%s %s (container %s): 占槽", p.Name, holder.State, holder.Name)
						if holder.State != state.ContainerCreated {
							text += "; 仍在跑 job，跑完后自动退出"
						}
						if code := writeCLIText(stdout, stderr, text+"\n"); code != 0 {
							return code
						}
					}
				}
			}
		}
		if runErr != nil {
			results = append(results, runtimeResult{Name: name, Outcome: "failed", Message: runErr.Error()})
			code = 1
			continue
		}
		results = append(results, runtimeResult{Name: name, Outcome: "done", Message: message})
	}
	if code != 0 && attempted {
		code = 3
	}
	if jsonOutput {
		if err := writeJSONLine(stdout, struct {
			Results []runtimeResult `json:"results"`
		}{Results: results}); err != nil {
			return writeOperationError(stderr, err)
		}
		return code
	}
	for _, result := range results {
		if result.Outcome == "failed" {
			if _, err := fmt.Fprintf(stderr, "%s profile %s: %s\n", cmd, result.Name, result.Message); err != nil {
				return 1
			}
			continue
		}
		if code := writeCLIText(stdout, stderr, result.Message+"\n"); code != 0 {
			return code
		}
	}
	return code
}

func finishFleetReport(ctx context.Context, stdout, stderr io.Writer, report app.FleetReport, err error, jsonOutput bool, text func() error) int {
	_ = ctx
	if report.Phase == "" {
		report.Phase = app.FleetPhasePlan
	}
	if report.Results == nil {
		report.Results = []app.FleetResult{}
	}
	if jsonOutput {
		if writeCode := writeFleetJSON(stdout, stderr, report); writeCode != 0 {
			return writeCode
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
		}
		return fleetExitCode(report, err)
	}
	if text != nil {
		if textErr := text(); textErr != nil {
			return writeOperationError(stderr, textErr)
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
	}
	return fleetExitCode(report, err)
}

func fleetExitCode(report app.FleetReport, err error) int {
	if errors.Is(err, app.ErrFleetInput) {
		return 2
	}
	if report.Phase == app.FleetPhaseExecute {
		if err == nil && report.Complete {
			return 0
		}
		return 3
	}
	if err != nil {
		return 1
	}
	return 0
}

func writeFleetJSON(stdout, stderr io.Writer, value any) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func writeJSONLine(stdout io.Writer, value any) error {
	encoder := json.NewEncoder(stdout)
	return encoder.Encode(value)
}

const maxBundleInputBytes = 1 << 20

func readBundleFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f, maxBundleInputBytes)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("bundle exceeds 1 MiB")
	}
	return data, nil
}
