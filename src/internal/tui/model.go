package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gha-runner-tui/internal/app"
	"gha-runner-tui/internal/config"
)

type screen int

const (
	screenDashboard screen = iota
	screenDetail
	screenLogs
	screenCreate
	screenConfirm
	screenHelp
)

type logKind int

const (
	logKindSystemd logKind = iota
	logKindDocker
)

type confirmAction int

const (
	actionStop confirmAction = iota
	actionRestart
	actionKill
	actionCleanup
)

type dashboardLoadedMsg struct {
	dashboard app.Dashboard
	err       error
}

type logsLoadedMsg struct {
	title   string
	content string
	kind    logKind
	err     error
}

type actionDoneMsg struct {
	status string
	err    error
	reload bool
}

type tickMsg time.Time

type confirmState struct {
	title   string
	body    string
	action  confirmAction
	profile string
}

type createField struct {
	label string
	input textinput.Model
}

type Model struct {
	manager app.RunnerManager

	screen screen
	width  int
	height int

	dashboard    app.Dashboard
	table        table.Model
	selectedName string

	loading       bool
	statusMessage string
	errorMessage  string

	logViewport viewport.Model
	logTitle    string
	logContent  string
	logKind     logKind
	logFollow   bool

	createFields []createField
	createFocus  int

	confirm *confirmState
}

func NewModel(manager app.RunnerManager) Model {
	columns := []table.Column{
		{Title: "Profile", Width: 22},
		{Title: "Service", Width: 10},
		{Title: "Loop", Width: 12},
		{Title: "Container", Width: 12},
		{Title: "GitHub", Width: 10},
		{Title: "Busy", Width: 6},
		{Title: "Health", Width: 10},
	}

	tbl := table.New(
		table.WithColumns(columns),
		table.WithRows(nil),
		table.WithFocused(true),
		table.WithHeight(12),
	)

	styles := table.DefaultStyles()
	styles.Header = styles.Header.BorderForeground(lipgloss.Color("240")).BorderBottom(true).Bold(true)
	styles.Selected = styles.Selected.Foreground(lipgloss.Color("230")).Background(lipgloss.Color("25")).Bold(true)
	tbl.SetStyles(styles)

	fields := newCreateFields()
	return Model{
		manager:      manager,
		screen:       screenDashboard,
		table:        tbl,
		logViewport:  viewport.New(80, 20),
		createFields: fields,
	}
}

func (m Model) Init() tea.Cmd {
	return loadDashboardCmd(m.manager)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.table.SetWidth(max(80, msg.Width-4))
		m.table.SetHeight(max(8, msg.Height-10))
		m.logViewport.Width = max(20, msg.Width-4)
		m.logViewport.Height = max(6, msg.Height-8)
		return m, nil

	case dashboardLoadedMsg:
		m.loading = false
		if msg.err != nil {
			m.errorMessage = msg.err.Error()
			return m, nil
		}
		m.errorMessage = ""
		m.dashboard = msg.dashboard
		m.syncTable()
		if m.screen == screenDetail && m.currentSnapshot() == nil {
			m.screen = screenDashboard
			m.statusMessage = "selected profile no longer exists"
		}
		return m, nil

	case logsLoadedMsg:
		if msg.err != nil {
			m.errorMessage = msg.err.Error()
			m.logContent = ""
		} else {
			m.errorMessage = ""
			m.logTitle = msg.title
			m.logKind = msg.kind
			m.logContent = msg.content
			m.logViewport.SetContent(msg.content)
		}
		if m.logFollow && m.screen == screenLogs {
			return m, tickCmd()
		}
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			m.errorMessage = msg.err.Error()
			return m, nil
		}
		m.errorMessage = ""
		m.statusMessage = msg.status
		if msg.reload {
			m.loading = true
			return m, loadDashboardCmd(m.manager)
		}
		return m, nil

	case tickMsg:
		if m.screen == screenLogs && m.logFollow {
			return m, m.loadCurrentLogsCmd()
		}
		return m, nil

	case tea.KeyMsg:
		switch m.screen {
		case screenDashboard:
			return m.updateDashboard(msg)
		case screenDetail:
			return m.updateDetail(msg)
		case screenLogs:
			return m.updateLogs(msg)
		case screenCreate:
			return m.updateCreate(msg)
		case screenConfirm:
			return m.updateConfirm(msg)
		case screenHelp:
			if msg.String() == "esc" || msg.String() == "b" || msg.String() == "q" || msg.String() == "?" {
				m.screen = screenDashboard
			}
			return m, nil
		}
	}

	if m.screen == screenDashboard {
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		m.captureSelection()
		return m, cmd
	}

	return m, nil
}

func (m Model) View() string {
	header := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")).Render("GitHub Actions Docker Runner Manager")
	body := ""

	switch m.screen {
	case screenDashboard:
		body = m.viewDashboard()
	case screenDetail:
		body = m.viewDetail()
	case screenLogs:
		body = m.viewLogs()
	case screenCreate:
		body = m.viewCreate()
	case screenConfirm:
		body = m.viewConfirm()
	case screenHelp:
		body = m.viewHelp()
	}

	footerLines := make([]string, 0, 3)
	if m.loading {
		footerLines = append(footerLines, lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("Loading..."))
	}
	if m.statusMessage != "" {
		footerLines = append(footerLines, lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Render(m.statusMessage))
	}
	if m.errorMessage != "" {
		footerLines = append(footerLines, lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(m.errorMessage))
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, "", body, "", strings.Join(footerLines, "\n"))
}

func (m Model) updateDashboard(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "?":
		m.screen = screenHelp
		return m, nil
	case "r":
		m.loading = true
		return m, loadDashboardCmd(m.manager)
	case "enter":
		if snapshot := m.currentSnapshot(); snapshot != nil {
			m.selectedName = snapshot.Profile.Name
			m.screen = screenDetail
		}
		return m, nil
	case "c":
		m.screen = screenCreate
		m.createFields = newCreateFields()
		m.createFocus = 0
		m.focusCreateField()
		return m, nil
	case "s":
		if snapshot := m.currentSnapshot(); snapshot != nil {
			return m, startLoopCmd(m.manager, snapshot.Profile)
		}
	case "x":
		if snapshot := m.currentSnapshot(); snapshot != nil {
			m.confirm = &confirmState{
				title:   "Stop loop service",
				body:    "Stop the selected loop service?",
				action:  actionStop,
				profile: snapshot.Profile.Name,
			}
			m.screen = screenConfirm
		}
		return m, nil
	case "R":
		if snapshot := m.currentSnapshot(); snapshot != nil {
			body := "Restart the selected loop service?"
			if snapshot.BusyState == "yes" {
				body = "This runner appears busy. Restarting may interrupt the current GitHub Actions job.\n\nRestart anyway?"
			}
			m.confirm = &confirmState{
				title:   "Restart loop service",
				body:    body,
				action:  actionRestart,
				profile: snapshot.Profile.Name,
			}
			m.screen = screenConfirm
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	m.captureSelection()
	return m, cmd
}

func (m Model) updateDetail(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	snapshot := m.currentSnapshot()
	if snapshot == nil {
		m.screen = screenDashboard
		return m, nil
	}

	switch msg.String() {
	case "esc", "b":
		m.screen = screenDashboard
		return m, nil
	case "q", "ctrl+c":
		return m, tea.Quit
	case "?":
		m.screen = screenHelp
		return m, nil
	case "r":
		m.loading = true
		return m, loadDashboardCmd(m.manager)
	case "j":
		m.screen = screenLogs
		m.logFollow = false
		return m, loadSystemdLogsCmd(m.manager, snapshot.Profile)
	case "d":
		m.screen = screenLogs
		m.logFollow = false
		return m, loadDockerLogsCmd(m.manager, *snapshot)
	case "s":
		return m, startLoopCmd(m.manager, snapshot.Profile)
	case "x":
		m.confirm = &confirmState{
			title:   "Stop loop service",
			body:    "Stop this loop service?",
			action:  actionStop,
			profile: snapshot.Profile.Name,
		}
		m.screen = screenConfirm
		return m, nil
	case "R":
		body := "Restart this loop service?"
		if snapshot.BusyState == "yes" {
			body = "This runner appears busy. Restarting may interrupt the current GitHub Actions job.\n\nRestart anyway?"
		}
		m.confirm = &confirmState{
			title:   "Restart loop service",
			body:    body,
			action:  actionRestart,
			profile: snapshot.Profile.Name,
		}
		m.screen = screenConfirm
		return m, nil
	case "k":
		body := "Kill the current runner container?"
		if snapshot.BusyState == "yes" {
			body = "This runner appears busy. Killing the container may fail the current GitHub Actions job.\n\nKill anyway?"
		}
		m.confirm = &confirmState{
			title:   "Kill runner container",
			body:    body,
			action:  actionKill,
			profile: snapshot.Profile.Name,
		}
		m.screen = screenConfirm
		return m, nil
	case "C":
		m.confirm = &confirmState{
			title:   "Cleanup stale resources",
			body:    "Remove exited containers that match this profile's prefix?",
			action:  actionCleanup,
			profile: snapshot.Profile.Name,
		}
		m.screen = screenConfirm
		return m, nil
	}

	return m, nil
}

func (m Model) updateLogs(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "b":
		m.screen = screenDetail
		m.logFollow = false
		return m, nil
	case "q", "ctrl+c":
		return m, tea.Quit
	case "r":
		return m, m.loadCurrentLogsCmd()
	case "f":
		m.logFollow = !m.logFollow
		if m.logFollow {
			return m, tea.Batch(m.loadCurrentLogsCmd(), tickCmd())
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.logViewport, cmd = m.logViewport.Update(msg)
	return m, cmd
}

func (m Model) updateCreate(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.screen = screenDashboard
		return m, nil
	case "ctrl+c", "q":
		return m, tea.Quit
	case "tab", "shift+tab", "up", "down":
		if msg.String() == "up" || msg.String() == "shift+tab" {
			m.createFocus--
		} else {
			m.createFocus++
		}
		if m.createFocus >= len(m.createFields) {
			m.createFocus = 0
		}
		if m.createFocus < 0 {
			m.createFocus = len(m.createFields) - 1
		}
		m.focusCreateField()
		return m, nil
	case "ctrl+s":
		input, err := m.readCreateInput()
		if err != nil {
			m.errorMessage = err.Error()
			return m, nil
		}
		m.screen = screenDashboard
		m.loading = true
		return m, createProfileCmd(m.manager, input)
	}

	var cmds []tea.Cmd
	for i := range m.createFields {
		var cmd tea.Cmd
		m.createFields[i].input, cmd = m.createFields[i].input.Update(msg)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "n", "esc":
		m.screen = screenDetail
		m.confirm = nil
		return m, nil
	case "y", "enter":
		confirm := m.confirm
		m.confirm = nil
		m.screen = screenDetail
		if confirm == nil {
			return m, nil
		}
		snapshot := m.snapshotByName(confirm.profile)
		if snapshot == nil {
			m.errorMessage = "profile no longer exists"
			return m, nil
		}
		switch confirm.action {
		case actionStop:
			return m, stopLoopCmd(m.manager, *snapshot)
		case actionRestart:
			return m, restartLoopCmd(m.manager, *snapshot)
		case actionKill:
			return m, killContainerCmd(m.manager, *snapshot)
		case actionCleanup:
			return m, cleanupCmd(m.manager, snapshot.Profile)
		}
	}
	return m, nil
}

func (m Model) viewDashboard() string {
	info := fmt.Sprintf("Profiles: %d", len(m.dashboard.Profiles))
	if len(m.dashboard.ProfileErrors) > 0 {
		info += fmt.Sprintf("  Invalid profiles: %d", len(m.dashboard.ProfileErrors))
	}
	help := "[r] refresh  [enter] details  [c] create  [s] start  [x] stop  [R] restart  [?] help  [q] quit"
	return lipgloss.JoinVertical(lipgloss.Left, info, "", m.table.View(), "", help)
}

func (m Model) viewDetail() string {
	snapshot := m.currentSnapshot()
	if snapshot == nil {
		return "No profile selected."
	}

	lastExitCode := "-"
	if snapshot.Loop.LastExitCode != nil {
		lastExitCode = strconv.Itoa(*snapshot.Loop.LastExitCode)
	}
	lastError := "-"
	if snapshot.Loop.LastError != nil && *snapshot.Loop.LastError != "" {
		lastError = *snapshot.Loop.LastError
	}
	lastTransition := "-"
	if !snapshot.Loop.LastTransitionAt.IsZero() {
		lastTransition = snapshot.Loop.LastTransitionAt.UTC().Format(time.RFC3339)
	}
	githubRunner := "-"
	if snapshot.GitHubRunner != nil {
		githubRunner = snapshot.GitHubRunner.Name
	} else if snapshot.Loop.LastRunnerName != "" {
		githubRunner = snapshot.Loop.LastRunnerName
	}
	containerName := "-"
	if snapshot.Container.Name != "" {
		containerName = snapshot.Container.Name
	} else if snapshot.Loop.LastContainerName != "" {
		containerName = snapshot.Loop.LastContainerName
	}
	containerID := "-"
	if snapshot.Container.ID != "" {
		containerID = snapshot.Container.ID
	} else if snapshot.Loop.LastContainerID != "" {
		containerID = snapshot.Loop.LastContainerID
	}

	lines := []string{
		fmt.Sprintf("Profile:            %s", snapshot.Profile.Name),
		fmt.Sprintf("Repository:         %s/%s", snapshot.Profile.Repo.Owner, snapshot.Profile.Repo.Name),
		fmt.Sprintf("Service:            %s", snapshot.Profile.Service.Name),
		fmt.Sprintf("Service state:      %s", snapshot.Service.Active),
		fmt.Sprintf("Loop state:         %s", snapshot.DisplayLoopState),
		fmt.Sprintf("Health:             %s", snapshot.Health),
		fmt.Sprintf("Container:          %s", containerName),
		fmt.Sprintf("Container ID:       %s", containerID),
		fmt.Sprintf("Docker image:       %s", dash(snapshot.Profile.Docker.Image)),
		fmt.Sprintf("GitHub runner:      %s", githubRunner),
		fmt.Sprintf("GitHub status:      %s", snapshot.GitHubState),
		fmt.Sprintf("GitHub busy:        %s", snapshot.BusyState),
		fmt.Sprintf("Last transition:    %s", lastTransition),
		fmt.Sprintf("Last exit code:     %s", lastExitCode),
		fmt.Sprintf("Last error:         %s", lastError),
	}
	if summary := snapshot.ErrorSummary(); summary != "" {
		lines = append(lines, "", "Observed errors:", summary)
	}

	help := "[j] systemd logs  [d] docker logs  [s] start  [x] stop  [R] restart  [k] kill  [C] cleanup  [b] back"
	return lipgloss.JoinVertical(lipgloss.Left, strings.Join(lines, "\n"), "", help)
}

func (m Model) viewLogs() string {
	title := m.logTitle
	if title == "" {
		title = "Logs"
	}
	mode := "manual refresh"
	if m.logFollow {
		mode = "auto refresh every 2s"
	}
	help := fmt.Sprintf("[r] refresh  [f] follow (%s)  [b] back", mode)
	return lipgloss.JoinVertical(lipgloss.Left, title, "", m.logViewport.View(), "", help)
}

func (m Model) viewCreate() string {
	lines := []string{"Create Profile", ""}
	for i, field := range m.createFields {
		label := field.label
		if i == m.createFocus {
			label = lipgloss.NewStyle().Foreground(lipgloss.Color("39")).Bold(true).Render(label)
		}
		lines = append(lines, fmt.Sprintf("%-22s %s", label+":", field.input.View()))
	}
	lines = append(lines, "", "[tab] next  [shift+tab] previous  [ctrl+s] create  [esc] cancel")
	return strings.Join(lines, "\n")
}

func (m Model) viewConfirm() string {
	if m.confirm == nil {
		return ""
	}
	body := lipgloss.NewStyle().Width(max(40, m.width-10)).Render(m.confirm.body)
	return lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214")).Render(m.confirm.title),
		"",
		body,
		"",
		"[y] confirm  [n] cancel",
	)
}

func (m Model) viewHelp() string {
	lines := []string{
		"Global:",
		"  q quit",
		"  r refresh",
		"  ? help",
		"  esc back",
		"",
		"Dashboard:",
		"  enter open selected profile detail",
		"  c create profile",
		"  s start selected loop",
		"  x stop selected loop",
		"  R restart selected loop",
		"",
		"Detail:",
		"  j view systemd logs",
		"  d view Docker logs",
		"  k kill current container",
		"  C cleanup exited containers",
		"",
		"Logs:",
		"  f toggle auto refresh",
		"",
		"Create:",
		"  ctrl+s write profile and service unit, then enable/start it",
	}
	return strings.Join(lines, "\n")
}

func (m *Model) syncTable() {
	rows := make([]table.Row, 0, len(m.dashboard.Profiles))
	for _, snapshot := range m.dashboard.Profiles {
		rows = append(rows, table.Row{
			snapshot.Profile.Name,
			string(snapshot.Service.Active),
			string(snapshot.DisplayLoopState),
			string(snapshot.Container.State),
			string(snapshot.GitHubState),
			string(snapshot.BusyState),
			string(snapshot.Health),
		})
	}
	m.table.SetRows(rows)
	m.restoreSelection()
}

func (m *Model) captureSelection() {
	if snapshot := m.currentSnapshot(); snapshot != nil {
		m.selectedName = snapshot.Profile.Name
	}
}

func (m *Model) restoreSelection() {
	if m.selectedName == "" {
		if len(m.dashboard.Profiles) > 0 {
			m.selectedName = m.dashboard.Profiles[0].Profile.Name
			m.table.SetCursor(0)
		}
		return
	}
	for i, snapshot := range m.dashboard.Profiles {
		if snapshot.Profile.Name == m.selectedName {
			m.table.SetCursor(i)
			return
		}
	}
	if len(m.dashboard.Profiles) > 0 {
		m.selectedName = m.dashboard.Profiles[0].Profile.Name
		m.table.SetCursor(0)
	}
}

func (m Model) currentSnapshot() *app.ProfileSnapshot {
	index := m.table.Cursor()
	if index < 0 || index >= len(m.dashboard.Profiles) {
		return nil
	}
	return &m.dashboard.Profiles[index]
}

func (m Model) snapshotByName(name string) *app.ProfileSnapshot {
	for i := range m.dashboard.Profiles {
		if m.dashboard.Profiles[i].Profile.Name == name {
			return &m.dashboard.Profiles[i]
		}
	}
	return nil
}

func newCreateFields() []createField {
	specs := []struct {
		label       string
		value       string
		placeholder string
	}{
		{label: "Profile name", placeholder: "remind-me-swift"},
		{label: "Repo owner", placeholder: "bigtomcat6"},
		{label: "Repo name", placeholder: "remind-me"},
		{label: "Runner labels", value: "self-hosted,linux,x64,docker"},
		{label: "Docker image", placeholder: "ghcr.io/example/actions-runner:latest"},
		{label: "Service name", placeholder: "gha-remind-me-swift.service"},
		{label: "Container prefix", placeholder: "gha-remind-me-swift"},
		{label: "CPU limit", value: "2"},
		{label: "Memory limit", value: "4g"},
		{label: "Ephemeral", value: "true"},
	}

	fields := make([]createField, 0, len(specs))
	for _, spec := range specs {
		input := textinput.New()
		input.SetValue(spec.value)
		input.Placeholder = spec.placeholder
		input.Width = 48
		fields = append(fields, createField{
			label: spec.label,
			input: input,
		})
	}
	fields[0].input.Focus()
	return fields
}

func (m *Model) focusCreateField() {
	for i := range m.createFields {
		if i == m.createFocus {
			m.createFields[i].input.Focus()
			continue
		}
		m.createFields[i].input.Blur()
	}
}

func (m Model) readCreateInput() (app.CreateProfileInput, error) {
	values := make(map[string]string, len(m.createFields))
	for _, field := range m.createFields {
		values[field.label] = strings.TrimSpace(field.input.Value())
	}

	ephemeral, err := parseBool(values["Ephemeral"])
	if err != nil {
		return app.CreateProfileInput{}, fmt.Errorf("ephemeral must be true or false")
	}

	labels := splitCSV(values["Runner labels"])
	input := app.CreateProfileInput{
		Name:                values["Profile name"],
		RepoOwner:           values["Repo owner"],
		RepoName:            values["Repo name"],
		RunnerLabels:        labels,
		DockerImage:         values["Docker image"],
		ServiceName:         values["Service name"],
		ContainerNamePrefix: values["Container prefix"],
		CPUs:                values["CPU limit"],
		Memory:              values["Memory limit"],
		Ephemeral:           ephemeral,
	}

	if input.Name == "" || input.RepoOwner == "" || input.RepoName == "" || input.DockerImage == "" || input.ServiceName == "" || input.ContainerNamePrefix == "" {
		return app.CreateProfileInput{}, fmt.Errorf("fill in all required fields before creating a profile")
	}

	return input, nil
}

func (m Model) loadCurrentLogsCmd() tea.Cmd {
	snapshot := m.currentSnapshot()
	if snapshot == nil {
		return nil
	}
	switch m.logKind {
	case logKindDocker:
		return loadDockerLogsCmd(m.manager, *snapshot)
	default:
		return loadSystemdLogsCmd(m.manager, snapshot.Profile)
	}
}

func loadDashboardCmd(manager app.RunnerManager) tea.Cmd {
	return func() tea.Msg {
		dashboard, err := manager.Dashboard(context.Background())
		return dashboardLoadedMsg{dashboard: dashboard, err: err}
	}
}

func loadSystemdLogsCmd(manager app.RunnerManager, profile config.Profile) tea.Cmd {
	return func() tea.Msg {
		content, err := manager.SystemdLogs(context.Background(), profile, 200, false)
		return logsLoadedMsg{
			title:   "systemd logs: " + profile.Service.Name,
			content: content,
			kind:    logKindSystemd,
			err:     err,
		}
	}
}

func loadDockerLogsCmd(manager app.RunnerManager, snapshot app.ProfileSnapshot) tea.Cmd {
	return func() tea.Msg {
		content, err := manager.DockerLogs(context.Background(), snapshot, 200, false)
		return logsLoadedMsg{
			title:   "docker logs: " + snapshot.Profile.Name,
			content: content,
			kind:    logKindDocker,
			err:     err,
		}
	}
}

func startLoopCmd(manager app.RunnerManager, profile config.Profile) tea.Cmd {
	return func() tea.Msg {
		err := manager.StartLoop(context.Background(), profile)
		return actionDoneMsg{status: "loop service started", err: err, reload: err == nil}
	}
}

func stopLoopCmd(manager app.RunnerManager, snapshot app.ProfileSnapshot) tea.Cmd {
	return func() tea.Msg {
		err := manager.StopLoop(context.Background(), snapshot)
		return actionDoneMsg{status: "loop service stopped", err: err, reload: err == nil}
	}
}

func restartLoopCmd(manager app.RunnerManager, snapshot app.ProfileSnapshot) tea.Cmd {
	return func() tea.Msg {
		err := manager.RestartLoop(context.Background(), snapshot)
		return actionDoneMsg{status: "loop service restarted", err: err, reload: err == nil}
	}
}

func killContainerCmd(manager app.RunnerManager, snapshot app.ProfileSnapshot) tea.Cmd {
	return func() tea.Msg {
		err := manager.KillContainer(context.Background(), snapshot)
		return actionDoneMsg{status: "runner container killed", err: err, reload: err == nil}
	}
}

func cleanupCmd(manager app.RunnerManager, profile config.Profile) tea.Cmd {
	return func() tea.Msg {
		removed, err := manager.CleanupExited(context.Background(), profile)
		return actionDoneMsg{status: app.FormatCleanupResult(removed), err: err, reload: err == nil}
	}
}

func createProfileCmd(manager app.RunnerManager, input app.CreateProfileInput) tea.Cmd {
	return func() tea.Msg {
		err := manager.CreateProfile(context.Background(), input)
		return actionDoneMsg{status: "profile created and service started", err: err, reload: err == nil}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			items = append(items, part)
		}
	}
	return items
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "1", "y":
		return true, nil
	case "false", "no", "0", "n":
		return false, nil
	default:
		return false, fmt.Errorf("invalid bool")
	}
}

func dash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
