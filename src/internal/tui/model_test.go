package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/config"
	"gha-runner-tui/internal/docker"
	"gha-runner-tui/internal/state"
	"gha-runner-tui/internal/systemd"
)

func TestReadCreateInputParsesOrganizationMode(t *testing.T) {
	t.Parallel()

	m := NewModel(testManager())
	m.createFields = newCreateFields()
	setCreateField(&m, "Target scope", "organization")
	setCreateField(&m, "Organization", "Example Org")
	setCreateField(&m, "Environment", "swift")
	setCreateField(&m, "Docker access", "rootless")
	setCreateField(&m, "Runner labels", "self-hosted,linux,x64,docker,swift")
	setCreateField(&m, "Docker image", "gha-runner-swift:latest")
	setCreateField(&m, "CPU limit", "2")
	setCreateField(&m, "Memory limit", "4g")
	setCreateField(&m, "Ephemeral", "true")

	input, err := m.readCreateInput()
	if err != nil {
		t.Fatalf("readCreateInput returned error: %v", err)
	}
	if input.Scope != config.TargetScopeOrganization {
		t.Fatalf("expected organization scope, got %q", input.Scope)
	}
	if input.Org != "Example Org" || input.Environment != "swift" {
		t.Fatalf("unexpected org input: %+v", input)
	}
	if input.DockerAccess != "rootless" {
		t.Fatalf("expected rootless docker access, got %q", input.DockerAccess)
	}
	if input.CPUs != "2" || input.Memory != "4g" || !input.Ephemeral {
		t.Fatalf("unexpected resources/ephemeral: %+v", input)
	}
}

func TestReadCreateInputParsesDockerAccess(t *testing.T) {
	t.Parallel()

	m := NewModel(testManager())
	m.createFields = newCreateFields()
	setCreateField(&m, "Docker access", "host-socket")
	setCreateField(&m, "Profile name", "remind-me-swift")
	setCreateField(&m, "Repo owner", "bigtomcat6")
	setCreateField(&m, "Repo name", "remind-me")
	setCreateField(&m, "Docker image", "gha-runner-base:latest")
	setCreateField(&m, "Service name", "gha-remind-me-swift.service")
	setCreateField(&m, "Container prefix", "gha-remind-me-swift")
	setCreateField(&m, "CPU limit", "3")
	setCreateField(&m, "Memory limit", "6g")

	input, err := m.readCreateInput()
	if err != nil {
		t.Fatalf("readCreateInput returned error: %v", err)
	}
	if input.DockerAccess != "host-socket" {
		t.Fatalf("expected host-socket docker access, got %q", input.DockerAccess)
	}
	if input.CPUs != "3" || input.Memory != "6g" || !input.Ephemeral {
		t.Fatalf("unexpected resources/ephemeral: %+v", input)
	}
}

func TestReadCreateInputRejectsInvalidDockerAccess(t *testing.T) {
	t.Parallel()

	m := NewModel(testManager())
	m.createFields = newCreateFields()
	setCreateField(&m, "Docker access", "invalid")
	setCreateField(&m, "Profile name", "remind-me-swift")
	setCreateField(&m, "Repo owner", "bigtomcat6")
	setCreateField(&m, "Repo name", "remind-me")
	setCreateField(&m, "Docker image", "gha-runner-base:latest")
	setCreateField(&m, "Service name", "gha-remind-me-swift.service")
	setCreateField(&m, "Container prefix", "gha-remind-me-swift")
	setCreateField(&m, "CPU limit", "2")
	setCreateField(&m, "Memory limit", "4g")

	_, err := m.readCreateInput()
	if err == nil || !strings.Contains(err.Error(), "docker access") {
		t.Fatalf("expected invalid docker access error, got %v", err)
	}
}

func TestCreateFormQIsInputNotQuit(t *testing.T) {
	m := NewModel(testManager())
	m.screen = screenCreate
	for i := range m.createFields {
		if m.createFields[i].label == "Profile name" {
			m.createFocus = i
		}
	}
	m.focusCreateField()
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	got := updated.(Model)
	if got.screen != screenCreate {
		t.Fatal("q left form")
	}
	for _, field := range got.createFields {
		if field.label == "Profile name" && field.input.Value() != "q" {
			t.Fatalf("q not entered: %q", field.input.Value())
		}
	}
	if cmd != nil {
		if _, quit := cmd().(tea.QuitMsg); quit {
			t.Fatal("q quit")
		}
	}
}

func TestCreateFormQuitAndCancel(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyCtrlC, tea.KeyEsc} {
		t.Run(tea.KeyMsg{Type: key}.String(), func(t *testing.T) {
			m := NewModel(testManager())
			m.screen = screenCreate
			updated, cmd := m.Update(tea.KeyMsg{Type: key})
			if key == tea.KeyCtrlC {
				if cmd == nil {
					t.Fatal("ctrl+c did not quit")
				}
				if _, quit := cmd().(tea.QuitMsg); !quit {
					t.Fatal("ctrl+c did not return QuitMsg")
				}
			} else if updated.(Model).screen != screenDashboard || cmd != nil {
				t.Fatal("esc must cancel without quitting or creating")
			}
		})
	}
}

func TestCreateResourcesInitiallyEmpty(t *testing.T) {
	m := NewModel(testManager())
	for _, field := range m.createFields {
		if (field.label == "CPU limit" || field.label == "Memory limit") && field.input.Value() != "" {
			t.Errorf("%s has a default: %q", field.label, field.input.Value())
		}
	}
}

func TestReadCreateInputRequiresResourcesAndLabels(t *testing.T) {
	for _, scope := range []string{"repository", "organization"} {
		for _, field := range []string{"CPU limit", "Memory limit", "Runner labels"} {
			t.Run(scope+"/"+field, func(t *testing.T) {
				m := filledCreateModel(scope)
				setCreateField(&m, field, "  ")
				if _, err := m.readCreateInput(); err == nil {
					t.Fatalf("accepted empty %s", field)
				}
			})
		}
	}
}

func TestReadCreateInputRequiresEphemeralTrue(t *testing.T) {
	for _, value := range []string{"false", "no", "0", "invalid", ""} {
		t.Run(value, func(t *testing.T) {
			m := filledCreateModel("repository")
			setCreateField(&m, "Ephemeral", value)
			if _, err := m.readCreateInput(); err == nil || !strings.Contains(err.Error(), "ephemeral must be true") {
				t.Fatalf("accepted non-ephemeral input %q: %v", value, err)
			}
		})
	}
}

func TestCreateViewExplainsOrganizationWatch(t *testing.T) {
	m := filledCreateModel("organization")
	if view := m.viewCreate(); !strings.Contains(view, "watch_repositories") || !strings.Contains(view, "before starting") {
		t.Fatalf("missing manual watch requirement: %s", view)
	}
	for _, field := range m.createFields {
		if strings.Contains(strings.ToLower(field.label), "watch") {
			t.Fatal("watch form field is outside M1")
		}
	}
}

func TestGracefulConfirmationsDashboardAndDetail(t *testing.T) {
	for _, key := range []string{"x", "R"} {
		for _, busy := range []state.BusyStatus{state.BusyYes, state.BusyNo} {
			t.Run(key+"/"+string(busy), func(t *testing.T) {
				var bodies []string
				for _, screen := range []screen{screenDashboard, screenDetail} {
					m := NewModel(testManager())
					m.screen = screen
					m.dashboard.Profiles = []app.ProfileSnapshot{{Profile: config.Profile{Name: "p"}, BusyState: busy}}
					m.syncTable()
					updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
					got := updated.(Model)
					if got.screen != screenConfirm || got.confirm == nil {
						t.Fatal("missing confirmation")
					}
					body := got.confirm.body
					if !strings.Contains(body, "停止后不再接新 job；正在运行的 job 会继续跑完") || strings.Contains(strings.ToLower(body), "may interrupt") {
						t.Errorf("incorrect graceful message: %s", body)
					}
					if key == "R" && (!strings.Contains(body, "Restart the loop") || !strings.Contains(body, "retained container")) {
						t.Errorf("restart must explain adoption: %s", body)
					}
					bodies = append(bodies, body)
				}
				if bodies[0] != bodies[1] {
					t.Fatalf("dashboard/detail disagree: %q", bodies)
				}
			})
		}
	}
}

func TestKillConfirmationRetainsBusyJobWarning(t *testing.T) {
	m := NewModel(testManager())
	m.screen = screenDetail
	m.dashboard.Profiles = []app.ProfileSnapshot{{Profile: config.Profile{Name: "p"}, BusyState: state.BusyYes}}
	m.syncTable()
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	got := updated.(Model)
	if got.confirm == nil || got.confirm.action != actionKill || !strings.Contains(got.confirm.body, "may fail the current GitHub Actions job") {
		t.Fatalf("missing destructive kill warning: %+v", got.confirm)
	}
}

func TestDashboardDisplaysWaitingHostAndFailedHealth(t *testing.T) {
	for _, tc := range []struct {
		loop   state.LoopStatus
		health state.CombinedHealth
	}{{state.LoopWaitingHost, state.HealthHealthy}, {state.LoopFailed, state.HealthUnhealthy}} {
		t.Run(string(tc.loop), func(t *testing.T) {
			m := NewModel(testManager())
			snapshot := app.ProfileSnapshot{Profile: config.Profile{Name: "p"}, DisplayLoopState: tc.loop, Health: tc.health}
			updated, _ := m.Update(dashboardLoadedMsg{dashboard: app.Dashboard{Profiles: []app.ProfileSnapshot{snapshot}}})
			got := updated.(Model)
			row := got.table.Rows()[0]
			if row[2] != string(tc.loop) || row[6] != string(tc.health) {
				t.Fatalf("wrong state/health: %v", row)
			}
			if detail := got.viewDetail(); !strings.Contains(detail, string(tc.loop)) || !strings.Contains(detail, string(tc.health)) {
				t.Fatalf("detail lost state/health: %s", detail)
			}
		})
	}
}

func TestCreatePermissionErrorRetainsSudoHint(t *testing.T) {
	m := NewModel(testManager())
	m.statusMessage = "profile created and service started"
	err := fmt.Errorf("YAML creation: %w; permission denied; run create with sudo (sudo gha-runner-tui create ...)", os.ErrPermission)
	updated, cmd := m.Update(actionDoneMsg{status: "profile created and service started", err: err})
	got := updated.(Model)
	if got.errorMessage != err.Error() || got.statusMessage != "" || cmd != nil {
		t.Fatalf("permission error swallowed or success shown: error=%q status=%q", got.errorMessage, got.statusMessage)
	}
}

type tuiRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f tuiRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestDockerLogsMessagesRedactedOrFailClosed(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(fmt.Sprint(unavailable), func(t *testing.T) {
			runner := tuiRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name != "docker" || len(args) < 3 || !reflect.DeepEqual(args[:2], []string{"--host", "unix:///var/run/docker.sock"}) {
					t.Fatalf("unexpected command: %s %v", name, args)
				}
				switch args[2] {
				case "ps":
					return []byte("id\tcontainer\timage\trunning\tp\trunner\n"), nil
				case "inspect":
					if unavailable {
						return []byte("raw-token inspect error"), errors.New("raw-token inspect error")
					}
					return []byte(`[{"Config":{"Env":["RUNNER_TOKEN=raw-token","REG_TOKEN=reg-token"]}}]`), nil
				case "logs":
					if unavailable {
						t.Fatal("logs read without Env gate")
					}
					if !reflect.DeepEqual(args[3:], []string{"--tail", "200", "container"}) {
						t.Fatalf("unexpected logs args: %v", args)
					}
					return []byte("job raw-token reg-token done"), nil
				default:
					t.Fatalf("unexpected Docker operation: %v", args)
					return nil, nil
				}
			})
			d := docker.NewClient(runner)
			manager := testManager()
			manager.Docker = d
			manager.Service.Docker = d
			m := NewModel(manager)
			m.screen = screenLogs
			m.logContent = "previous logs"
			m.logViewport.SetContent(m.logContent)
			msg := loadDockerLogsCmd(manager, app.ProfileSnapshot{Profile: config.Profile{Name: "p"}})()
			updated, _ := m.Update(msg)
			got := updated.(Model)
			if unavailable {
				if got.errorMessage != "cannot obtain container log redaction data" || got.logContent != "" || strings.Contains(got.View(), "previous logs") {
					t.Fatalf("unsafe/stale failed log view: error=%q content=%q view=%q", got.errorMessage, got.logContent, got.View())
				}
			} else if got.errorMessage != "" || got.logContent != "job [REDACTED] [REDACTED] done" || strings.Contains(got.View(), "raw-token") {
				t.Fatalf("unexpected successful logs: error=%q content=%q", got.errorMessage, got.logContent)
			}
		})
	}
}

func TestJournalEntryIndependentOfDockerGate(t *testing.T) {
	runner := tuiRunnerFunc(func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "journalctl" || !reflect.DeepEqual(args, []string{"-u", "gha-p.service", "-n", "200", "--no-pager"}) {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return []byte("safe journal output"), nil
	})
	manager := testManager()
	manager.Systemd = systemd.NewClient(runner)
	m := NewModel(manager)
	m.screen = screenDetail
	m.dashboard.Profiles = []app.ProfileSnapshot{{Profile: config.Profile{Name: "p", Service: config.ServiceConfig{Name: "gha-p.service"}}}}
	m.syncTable()
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if cmd == nil {
		t.Fatal("missing journal command")
	}
	updated, _ = updated.(Model).Update(cmd())
	got := updated.(Model)
	if got.screen != screenLogs || got.logKind != logKindSystemd || got.logContent != "safe journal output" || got.errorMessage != "" {
		t.Fatalf("journal blocked: %+v", got)
	}
}

func filledCreateModel(scope string) Model {
	m := NewModel(testManager())
	for label, value := range map[string]string{
		"Target scope": scope, "Organization": "Example Org", "Environment": "swift",
		"Profile name": "p", "Repo owner": "owner", "Repo name": "repo",
		"Docker image": "runner:latest", "Service name": "gha-p.service", "Container prefix": "gha-p",
		"CPU limit": "2", "Memory limit": "4g",
	} {
		setCreateField(&m, label, value)
	}
	return m
}

func testManager() app.RunnerManager {
	return app.RunnerManager{}
}

func setCreateField(m *Model, label, value string) {
	for i := range m.createFields {
		if m.createFields[i].label == label {
			m.createFields[i].input.SetValue(value)
			return
		}
	}
}
