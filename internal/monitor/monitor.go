// Package monitor is the read-only dashboard (the second surface). It opens the
// same SQLite store the environment writes to and shows live worker/task status
// — it never interacts with workers. Two modes: Once (a plain snapshot) and Run
// (a live bubbletea TUI polling ~1s). Run it in a pane beside the PM session.
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

// Once prints a single plain-text snapshot and returns.
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
	fmt.Print(render(name, tasks, false, 100))
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
	m := model{store: st, projectID: projectID, name: name, width: 100}
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
	err       error
	width     int
	height    int
}

type tasksMsg struct {
	tasks []*store.Task
	err   error
}
type tickMsg struct{}

func (m model) fetch() tea.Cmd {
	return func() tea.Msg {
		ts, err := m.store.ListTasks(m.projectID)
		return tasksMsg{ts, err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m model) Init() tea.Cmd { return tea.Batch(m.fetch(), tick()) }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tasksMsg:
		m.tasks, m.err = msg.tasks, msg.err
	case tickMsg:
		return m, tea.Batch(m.fetch(), tick())
	}
	return m, nil
}

func (m model) View() string {
	if m.err != nil {
		return fmt.Sprintf("error reading store: %v\n(press q to quit)\n", m.err)
	}
	return render(m.name, m.tasks, true, m.width) +
		lipgloss.NewStyle().Faint(true).Render("\n  q to quit · refreshes every 1s\n")
}

// --- rendering (shared by Once and the live View) ---

var (
	headerStyle = lipgloss.NewStyle().Bold(true)
	faintStyle  = lipgloss.NewStyle().Faint(true)
	statusColor = map[store.Status]lipgloss.Color{
		store.Todo:       lipgloss.Color("245"), // grey
		store.Running:    lipgloss.Color("39"),  // blue
		store.NeedsInput: lipgloss.Color("214"), // orange
		store.Done:       lipgloss.Color("42"),  // green
		store.Failed:     lipgloss.Color("203"), // red
		store.Blocked:    lipgloss.Color("245"),
	}
)

func render(name string, tasks []*store.Task, color bool, width int) string {
	var b strings.Builder
	counts := map[store.Status]int{}
	for _, t := range tasks {
		counts[t.Status]++
	}
	title := fmt.Sprintf("rambl · %s — %d tasks  (%d running, %d need input, %d done, %d failed)",
		name, len(tasks), counts[store.Running], counts[store.NeedsInput], counts[store.Done], counts[store.Failed])
	if color {
		title = headerStyle.Render(title)
	}
	b.WriteString(title + "\n\n")

	if len(tasks) == 0 {
		b.WriteString("  (no tasks yet)\n")
		return b.String()
	}

	hdr := fmt.Sprintf("  %-11s %-18s %-14s %-6s %s", "STATUS", "TASK", "DEPS", "AGE", "DETAIL")
	if color {
		hdr = faintStyle.Render(hdr)
	}
	b.WriteString(hdr + "\n")

	for _, t := range tasks {
		status := string(t.Status)
		statusCell := fmt.Sprintf("%-11s", status)
		if color {
			if c, ok := statusColor[t.Status]; ok {
				statusCell = lipgloss.NewStyle().Foreground(c).Render(statusCell)
			}
		}
		deps := "-"
		if len(t.Deps) > 0 {
			deps = strings.Join(t.Deps, ",")
		}
		detail := detailOf(t)
		// keep the row within width
		fixed := 2 + 11 + 1 + 18 + 1 + 14 + 1 + 6 + 1
		if max := width - fixed; max > 8 && len(detail) > max {
			detail = detail[:max-1] + "…"
		}
		b.WriteString(fmt.Sprintf("  %s %-18s %-14s %-6s %s\n",
			statusCell, truncate(t.Slug, 18), truncate(deps, 14), age(t.UpdatedAt), detail))
	}
	return b.String()
}

func detailOf(t *store.Task) string {
	if t.Status == store.NeedsInput && t.Question != "" {
		return "? " + firstLine(t.Question)
	}
	return firstLine(t.Result)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
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
