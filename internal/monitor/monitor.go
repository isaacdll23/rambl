// Package monitor is the read-only dashboard (the second surface). It opens the
// same SQLite store the environment writes to and shows live worker/task status
// — it never interacts with workers. Two modes: Once (a plain static snapshot)
// and Run (a live htop-style bubbletea TUI). Run it in a pane beside the PM
// session. The dashboard polls task/event data once a second and advances a
// local animation frame ~8×/second so spinners and the idle indicator breathe
// without hammering the database.
package monitor

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"rambl/internal/store"
)

// Once prints a single plain, static (non-animated) snapshot and returns.
func Once(dbPath, repoPath string) error {
	st, projectID, name, err := openProject(dbPath, repoPath)
	if err != nil {
		return err
	}
	defer st.Close()
	if projectID == "" {
		fmt.Printf("no rambl project for %s yet — start one with `rambl`\n", repoPath)
		return nil
	}
	tasks, err := st.ListTasks(projectID)
	if err != nil {
		return err
	}
	events, err := st.RecentEvents(projectID, 20)
	if err != nil {
		return err
	}
	features, _ := st.ListFeatures(projectID) // nil on error degrades to a flat list
	fmt.Print(render(view{
		name:      name,
		tasks:     tasks,
		events:    events,
		features:  features,
		startedAt: time.Now(),
		width:     100,
		animate:   false,
	}))
	return nil
}

// Run starts the live TUI and blocks until the user quits (q / ctrl-c / esc).
func Run(dbPath, repoPath string) error {
	st, projectID, name, err := openProject(dbPath, repoPath)
	if err != nil {
		return err
	}
	if projectID == "" {
		fmt.Printf("no rambl project for %s yet — start one with `rambl`\n", repoPath)
		st.Close()
		return nil
	}
	m := model{store: st, projectID: projectID, name: name, width: 100, startedAt: time.Now()}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	st.Close()
	return err
}

func openProject(dbPath, repoPath string) (*store.Store, string, string, error) {
	repo, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, "", "", err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, "", "", err
	}
	id, err := st.ProjectID(repo)
	if err != nil {
		st.Close()
		return nil, "", "", err
	}
	return st, id, filepath.Base(repo), nil
}

// --- bubbletea model ---

type model struct {
	store     *store.Store
	projectID string
	name      string
	tasks     []*store.Task
	events    []*store.Event
	features  []*store.Feature
	err       error
	width     int
	height    int
	frame     int       // animation frame, advanced by the anim tick
	selected  int       // cursor index into the worker rows
	startedAt time.Time // for "uptime"
}

// dataMsg carries a fresh poll of both tasks and events.
type dataMsg struct {
	tasks    []*store.Task
	events   []*store.Event
	features []*store.Feature
	err      error
}

// dataTickMsg fires once a second and triggers a DB re-fetch.
type dataTickMsg struct{}

// animTickMsg fires ~8×/second and only advances the local animation frame.
type animTickMsg struct{}

func (m model) fetch() tea.Cmd {
	return func() tea.Msg {
		ts, err := m.store.ListTasks(m.projectID)
		if err != nil {
			return dataMsg{err: err}
		}
		ev, err := m.store.RecentEvents(m.projectID, 20)
		ft, _ := m.store.ListFeatures(m.projectID) // nil on error degrades to a flat list
		return dataMsg{tasks: ts, events: ev, features: ft, err: err}
	}
}

func dataTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return dataTickMsg{} })
}

func animTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg { return animTickMsg{} })
}

func (m model) Init() tea.Cmd { return tea.Batch(m.fetch(), dataTick(), animTick()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "up", "k":
			if m.selected > 0 {
				m.selected--
			}
		case "down", "j":
			if m.selected < len(m.tasks)-1 {
				m.selected++
			}
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case dataMsg:
		m.tasks, m.events, m.features, m.err = msg.tasks, msg.events, msg.features, msg.err
		// clamp the cursor if the task count shrank between polls
		if m.selected >= len(m.tasks) {
			m.selected = len(m.tasks) - 1
		}
		if m.selected < 0 {
			m.selected = 0
		}
	case dataTickMsg:
		return m, tea.Batch(m.fetch(), dataTick())
	case animTickMsg:
		m.frame++
		return m, animTick()
	}
	return m, nil
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("error reading store: %v\n(press q to quit)\n", m.err)
	}
	return render(view{
		name:      m.name,
		tasks:     m.tasks,
		events:    m.events,
		features:  m.features,
		frame:     m.frame,
		selected:  m.selected,
		startedAt: m.startedAt,
		width:     m.width,
		height:    m.height,
		animate:   true,
	})
}

// --- rendering (shared by Once and the live View) ---

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	faintStyle  = lipgloss.NewStyle().Faint(true)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	statusColor = map[store.Status]lipgloss.Color{
		store.Todo:       lipgloss.Color("245"), // grey
		store.Running:    lipgloss.Color("39"),  // blue
		store.NeedsInput: lipgloss.Color("214"), // orange
		store.Done:       lipgloss.Color("42"),  // green
		store.Failed:     lipgloss.Color("203"), // red
		store.Blocked:    lipgloss.Color("245"),
	}
	spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	idleFrames    = []string{"·", "•", "●", "•"}
	// featureStatusColor maps a feature's lifecycle to the same palette the task
	// statuses use, so a feature header reads at a glance alongside its rows.
	featureStatusColor = map[store.FeatureStatus]lipgloss.Color{
		store.FeaturePlanning:    lipgloss.Color("245"), // grey
		store.FeatureRunning:     lipgloss.Color("39"),  // blue
		store.FeatureIntegrating: lipgloss.Color("214"), // orange
		store.FeatureDone:        lipgloss.Color("42"),  // green
		store.FeatureFailed:      lipgloss.Color("203"), // red
	}
)

// view is the full set of inputs the pure renderer needs. Keeping it a plain
// struct lets both View() and the tests drive render() without a live terminal.
type view struct {
	name      string
	tasks     []*store.Task
	events    []*store.Event
	features  []*store.Feature
	frame     int
	selected  int
	startedAt time.Time
	width     int
	height    int
	animate   bool
}

func render(v view) string {
	width := v.width
	if width <= 0 {
		width = 80
	}

	// Idle splash: nothing to show at all.
	if len(v.tasks) == 0 {
		return renderSplash(v.name, v.frame, v.animate, width, v.height)
	}

	var b strings.Builder
	b.WriteString(renderHeader(v.name, v.tasks, v.startedAt, width))
	b.WriteString("\n")
	b.WriteString(renderWorkers(v, width))
	b.WriteString("\n")
	b.WriteString(renderActivity(v.events))
	if v.animate {
		b.WriteString("\n")
		b.WriteString(faintStyle.Render("q quit · ↑↓ select · refreshes 1s"))
		b.WriteString("\n")
	}
	return b.String()
}

func renderSplash(name string, frame int, animate bool, width, height int) string {
	dot := idleFrames[0]
	if animate {
		dot = idleFrames[frame%len(idleFrames)]
	}
	block := lipgloss.JoinVertical(lipgloss.Center,
		headerStyle.Render("rambl · "+name),
		"",
		faintStyle.Render(dot+" idle · waiting for work"),
	)
	if height <= 0 {
		return block + "\n"
	}
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, block)
}

func renderHeader(name string, tasks []*store.Task, startedAt time.Time, width int) string {
	inner := maxInt(width-4, 10)
	left := headerStyle.Render("rambl · " + name)
	right := faintStyle.Render("uptime " + age(startedAt))
	titleLine := padBetween(left, right, inner)
	gaugeLine := renderGauges(tasks, inner)
	content := lipgloss.JoinVertical(lipgloss.Left, titleLine, gaugeLine)
	return boxStyle.Width(maxInt(width-2, 12)).Render(content) + "\n"
}

func renderGauges(tasks []*store.Task, width int) string {
	total := len(tasks)
	counts := map[store.Status]int{}
	for _, t := range tasks {
		counts[t.Status]++
	}
	order := []struct {
		st    store.Status
		label string
	}{
		{store.Running, "RUN"},
		{store.NeedsInput, "NEED"},
		{store.Done, "DONE"},
		{store.Failed, "FAIL"},
		{store.Todo, "TODO"},
		{store.Blocked, "BLK"},
	}
	var parts []string
	for _, o := range order {
		c := counts[o.st]
		if c == 0 {
			continue
		}
		seg := lipgloss.NewStyle().Foreground(statusColor[o.st]).Render(bar(c, total, 6))
		parts = append(parts, fmt.Sprintf("%s %s %d", o.label, seg, c))
	}
	if len(parts) == 0 {
		return faintStyle.Render("no tasks")
	}
	return strings.Join(parts, "   ") + faintStyle.Render(fmt.Sprintf("   ·  %d total", total))
}

// bar renders a proportional gauge: ▓ for the filled portion, ░ for the rest.
func bar(count, total, width int) string {
	if width <= 0 {
		return ""
	}
	if total <= 0 {
		return strings.Repeat("░", width)
	}
	filled := count * width / total
	if filled > width {
		filled = width
	}
	if count > 0 && filled == 0 {
		filled = 1 // never show a present status as wholly empty
	}
	return strings.Repeat("▓", filled) + strings.Repeat("░", width-filled)
}

func renderWorkers(v view, width int) string {
	var b strings.Builder
	counts := map[store.Status]int{}
	for _, t := range v.tasks {
		counts[t.Status]++
	}

	b.WriteString(headerStyle.Render("WORKERS"))
	b.WriteString("\n")
	if counts[store.Running] == 0 {
		spin := spinnerFrames[0]
		if v.animate {
			spin = spinnerFrames[v.frame%len(spinnerFrames)]
		}
		last := "—"
		if len(v.events) > 0 {
			last = age(v.events[0].CreatedAt)
		}
		b.WriteString(faintStyle.Render(fmt.Sprintf("  %s idle · waiting for a worker …   last PM action: %s", spin, last)))
		b.WriteString("\n")
	}

	// detail column gets whatever width is left after the fixed columns.
	const fixed = 2 + 1 + 1 + 11 + 1 + 16 + 1 + 5 + 1
	detailMax := maxInt(width-fixed, 8)

	// Build the grouped display order. `selected` indexes this flat sequence.
	groups := groupTasks(v.tasks, v.features)
	grouped := false
	for _, grp := range groups {
		if grp.feature != nil {
			grouped = true
			break
		}
	}

	idx := 0 // running position in the flat grouped order, matched against v.selected
	for _, grp := range groups {
		if grouped {
			if grp.feature != nil {
				b.WriteString(renderFeatureHeader(grp.feature))
			} else {
				b.WriteString(faintStyle.Render("▸ standalone"))
			}
			b.WriteString("\n")
		}
		for _, t := range grp.tasks {
			g := lipgloss.NewStyle().Foreground(statusColor[t.Status]).Render(glyph(t.Status, v.frame, v.animate))
			statusCell := lipgloss.NewStyle().Foreground(statusColor[t.Status]).Render(fmt.Sprintf("%-11s", string(t.Status)))
			slug := fmt.Sprintf("%-16s", truncate(t.Slug, 16))
			row := fmt.Sprintf("%s %s %s %-5s %s", g, statusCell, slug, age(t.UpdatedAt), truncate(detailOf(t), detailMax))

			marker := "  "
			if idx == v.selected {
				marker = lipgloss.NewStyle().Foreground(statusColor[t.Status]).Bold(true).Render("▌ ")
				row = lipgloss.NewStyle().Bold(true).Render(row)
			}
			indent := ""
			if grouped {
				indent = "  " // nest rows under their feature header
			}
			b.WriteString(indent + marker + row + "\n")

			if idx == v.selected {
				b.WriteString(renderExpanded(t, width))
			}
			idx++
		}
	}
	return b.String()
}

// taskGroup is one bucket in the grouped WORKERS view: a feature (nil for the
// standalone bucket) and the tasks that belong to it.
type taskGroup struct {
	feature *store.Feature
	tasks   []*store.Task
}

// groupTasks orders tasks for display: each feature (slug order, only those that
// own at least one task) followed by its rows, then the standalone tasks. A task
// whose FeatureID references a feature not in the list falls back to standalone
// rather than being dropped. With no features at all, it returns a single
// standalone group, which the renderer prints flat (no headers, no indent).
func groupTasks(tasks []*store.Task, features []*store.Feature) []taskGroup {
	byFeature := map[string][]*store.Task{}
	known := map[string]bool{}
	for _, f := range features {
		known[f.ID] = true
	}
	var standalone []*store.Task
	for _, t := range tasks {
		if t.FeatureID != "" && known[t.FeatureID] {
			byFeature[t.FeatureID] = append(byFeature[t.FeatureID], t)
		} else {
			standalone = append(standalone, t)
		}
	}
	var groups []taskGroup
	for _, f := range features {
		ts := byFeature[f.ID]
		if len(ts) == 0 {
			continue
		}
		groups = append(groups, taskGroup{feature: f, tasks: ts})
	}
	if len(standalone) > 0 {
		groups = append(groups, taskGroup{tasks: standalone})
	}
	return groups
}

// renderFeatureHeader prints the `▸ feat <slug>  <status>  <branch>` line that
// introduces a feature's rows, colored by the feature's lifecycle status.
func renderFeatureHeader(f *store.Feature) string {
	branch := f.Branch
	if branch == "" {
		branch = "-"
	}
	head := lipgloss.NewStyle().Foreground(featureStatusColor[f.Status]).Bold(true).
		Render(fmt.Sprintf("▸ feat %s", f.Slug))
	return head + faintStyle.Render(fmt.Sprintf("  %s  %s", string(f.Status), branch))
}

func renderExpanded(t *store.Task, width int) string {
	inner := maxInt(width-6, 12)
	full := strings.TrimSpace(fullDetailOf(t))
	if full == "" {
		full = "(no detail yet)"
	}
	deps := "-"
	if len(t.Deps) > 0 {
		deps = strings.Join(t.Deps, ", ")
	}
	branch := t.Branch
	if branch == "" {
		branch = "-"
	}
	body := lipgloss.JoinVertical(lipgloss.Left,
		lipgloss.NewStyle().Width(inner).Render(full),
		faintStyle.Render(fmt.Sprintf("branch: %s   deps: %s", branch, deps)),
	)
	return lipgloss.NewStyle().PaddingLeft(4).Render(body) + "\n"
}

func renderActivity(events []*store.Event) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("PM ACTIVITY"))
	b.WriteString("\n")
	if len(events) == 0 {
		b.WriteString(faintStyle.Render("  (no PM activity yet)"))
		b.WriteString("\n")
		return b.String()
	}
	n := len(events)
	if n > 8 {
		n = 8
	}
	for _, e := range events[:n] {
		b.WriteString(faintStyle.Render(fmt.Sprintf("  %-4s %s", age(e.CreatedAt), e.Summary)))
		b.WriteString("\n")
	}
	return b.String()
}

// glyph returns the per-status indicator: an animated braille spinner for
// running tasks (frame 0 when not animating), a static rune otherwise.
func glyph(s store.Status, frame int, animate bool) string {
	if s == store.Running {
		if animate {
			return spinnerFrames[frame%len(spinnerFrames)]
		}
		return spinnerFrames[0]
	}
	switch s {
	case store.NeedsInput:
		return "⚠"
	case store.Done:
		return "✓"
	case store.Failed:
		return "✗"
	case store.Blocked:
		return "⏸"
	default: // Todo and anything unknown
		return "•"
	}
}

// detailOf is the single-line detail shown in a worker row.
func detailOf(t *store.Task) string {
	if t.Status == store.NeedsInput && t.Question != "" {
		return "? " + firstLine(t.Question)
	}
	return firstLine(t.Result)
}

// fullDetailOf is the un-truncated detail shown in the expanded panel.
func fullDetailOf(t *store.Task) string {
	if t.Status == store.NeedsInput && t.Question != "" {
		return t.Question
	}
	return t.Result
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// padBetween left-justifies left and right-justifies right within w columns,
// counting display width (ANSI-aware) so styled strings still align.
func padBetween(left, right string, w int) string {
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

func age(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
