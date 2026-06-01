// Package picker is the interactive repository chooser (the entry surface). It
// opens a bubbletea TUI that lets you pick a git repository to launch rambl in:
// from the list of known projects (most-recently-opened first), by typing or
// pasting an arbitrary path, or by browsing the filesystem. Whatever you choose
// is validated against `git rev-parse --show-toplevel`, so the path it returns
// is always the top level of a git work tree.
package picker

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"rambl/internal/store"
	"rambl/internal/theme"
)

// Pick opens an interactive TUI to choose a repository to run rambl in.
// It lists known projects from the store at dbPath (most-recently-opened
// first), and offers a text field to enter an arbitrary path plus a simple
// filesystem browser. It returns the chosen repository's absolute path, or
// an empty string if the user cancelled (q / esc / ctrl-c). The returned
// path is guaranteed to be the top level of a git work tree.
func Pick(dbPath string) (string, error) {
	projects := loadProjects(dbPath)

	m := newModel(projects)
	p := tea.NewProgram(m, tea.WithAltScreen())
	final, err := p.Run()
	if err != nil {
		return "", err
	}
	fm, ok := final.(model)
	if !ok {
		return "", nil
	}
	return fm.result, nil
}

// loadProjects reads known projects from the store, most-recently-opened first.
// A missing or unreadable DB is not fatal — the picker still works for browsing
// and typing a path, so we just return an empty list.
func loadProjects(dbPath string) []*store.Project {
	st, err := store.Open(dbPath)
	if err != nil {
		return nil
	}
	defer st.Close()
	projects, err := st.ListProjects()
	if err != nil {
		return nil
	}
	return projects
}

// --- modes ---

type mode int

const (
	modeRecent mode = iota
	modePath
	modeBrowse
)

// --- model ---

type model struct {
	mode mode

	projects []*store.Project
	recentIx int

	input string // path-entry buffer

	browseDir   string
	entries     []browseEntry
	browseIx    int
	browseError string

	errMsg string // inline validation error shown to the user
	result string // chosen toplevel; set only on a successful validation
	width  int
	height int
}

type browseEntry struct {
	name  string // display name ("..", or a directory name)
	path  string // absolute path this entry points at
	isUp  bool
	isGit bool
}

func newModel(projects []*store.Project) model {
	m := model{
		mode:      modeRecent,
		projects:  projects,
		browseDir: startDir(),
	}
	m.loadBrowse(m.browseDir)
	return m
}

func startDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if cwd, err := os.Getwd(); err == nil {
		return cwd
	}
	return "/"
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// ctrl+c always quits, in every mode (returns "").
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}

	switch m.mode {
	case modePath:
		return m.handlePathKey(msg)
	case modeBrowse:
		return m.handleBrowseKey(msg)
	default:
		return m.handleRecentKey(msg)
	}
}

// --- RECENT mode ---

func (m model) handleRecentKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "p":
		m.mode = modePath
		m.errMsg = ""
		return m, nil
	case "b":
		m.mode = modeBrowse
		m.errMsg = ""
		return m, nil
	case "r":
		return m, nil
	case "up", "k":
		if m.recentIx > 0 {
			m.recentIx--
		}
		return m, nil
	case "down", "j":
		if m.recentIx < len(m.projects)-1 {
			m.recentIx++
		}
		return m, nil
	case "enter":
		if len(m.projects) == 0 {
			return m, nil
		}
		return m.trySelect(m.projects[m.recentIx].Path)
	}
	return m, nil
}

// --- PATH mode ---

func (m model) handlePathKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.mode = modeRecent
		m.errMsg = ""
		return m, nil
	case tea.KeyEnter:
		if strings.TrimSpace(m.input) == "" {
			return m, nil
		}
		return m.trySelect(m.input)
	case tea.KeyBackspace, tea.KeyDelete:
		if n := len(m.input); n > 0 {
			r := []rune(m.input)
			m.input = string(r[:len(r)-1])
		}
		return m, nil
	case tea.KeyRunes, tea.KeySpace:
		m.input += string(msg.Runes)
		return m, nil
	}
	return m, nil
}

// --- BROWSE mode ---

func (m model) handleBrowseKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "r":
		m.mode = modeRecent
		m.errMsg = ""
		return m, nil
	case "p":
		m.mode = modePath
		m.errMsg = ""
		return m, nil
	case "up", "k":
		if m.browseIx > 0 {
			m.browseIx--
		}
		return m, nil
	case "down", "j":
		if m.browseIx < len(m.entries)-1 {
			m.browseIx++
		}
		return m, nil
	case "enter":
		if len(m.entries) == 0 {
			return m, nil
		}
		m.loadBrowse(m.entries[m.browseIx].path)
		return m, nil
	case "o":
		if len(m.entries) == 0 {
			return m, nil
		}
		return m.trySelect(m.entries[m.browseIx].path)
	}
	return m, nil
}

// loadBrowse populates the directory listing for dir (directories only), with a
// ".." entry to go up. Directories that are git repos are flagged.
func (m *model) loadBrowse(dir string) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		m.browseError = err.Error()
		return
	}
	ents, err := os.ReadDir(abs)
	if err != nil {
		m.browseError = err.Error()
		return
	}
	m.browseError = ""
	m.browseDir = abs
	m.browseIx = 0

	var out []browseEntry
	if parent := filepath.Dir(abs); parent != abs {
		out = append(out, browseEntry{name: "..", path: parent, isUp: true})
	}

	var dirs []browseEntry
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // hide dotfiles to keep the list clean
		}
		full := filepath.Join(abs, name)
		dirs = append(dirs, browseEntry{name: name, path: full, isGit: isGitRepo(full)})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].name) < strings.ToLower(dirs[j].name)
	})
	m.entries = append(out, dirs...)
}

func isGitRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// --- selection / validation ---

func (m model) trySelect(path string) (tea.Model, tea.Cmd) {
	top, err := validateRepo(path)
	if err != nil {
		m.errMsg = err.Error()
		return m, nil
	}
	m.result = top
	m.errMsg = ""
	return m, tea.Quit
}

// normalizePath expands a leading "~" to the home dir and returns an absolute,
// cleaned path.
func normalizePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			path = home
		} else {
			path = filepath.Join(home, path[2:])
		}
	}
	return filepath.Abs(path)
}

// validateRepo resolves path to an absolute path (expanding "~"), then confirms
// it is within a git work tree by running `git -C <path> rev-parse
// --show-toplevel`. On success it returns the trimmed toplevel; otherwise it
// returns an error describing why the selection is invalid.
func validateRepo(path string) (string, error) {
	abs, err := normalizePath(path)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "-C", abs, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", &notRepoError{path: abs}
	}
	top := strings.TrimSpace(string(out))
	if top == "" {
		return "", &notRepoError{path: abs}
	}
	return top, nil
}

type notRepoError struct{ path string }

func (e *notRepoError) Error() string { return "not a git repository: " + e.path }

// --- view ---

var (
	titleStyle    = theme.Header
	faintStyle    = theme.Faint
	selectedStyle = lipgloss.NewStyle().Foreground(theme.Blue).Bold(true)
	gitStyle      = lipgloss.NewStyle().Foreground(theme.Green)
	errStyle      = lipgloss.NewStyle().Foreground(theme.Red)
	cursorStyle   = lipgloss.NewStyle().Foreground(theme.Blue)
)

func (m model) View() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("rambl · choose a repository") + "\n\n")

	switch m.mode {
	case modePath:
		b.WriteString(m.viewPath())
	case modeBrowse:
		b.WriteString(m.viewBrowse())
	default:
		b.WriteString(m.viewRecent())
	}

	if m.errMsg != "" {
		b.WriteString("\n" + errStyle.Render("✗ "+m.errMsg) + "\n")
	}

	b.WriteString("\n" + m.footer())
	return b.String()
}

func (m model) viewRecent() string {
	var b strings.Builder
	b.WriteString(faintStyle.Render("recent projects") + "\n\n")
	if len(m.projects) == 0 {
		b.WriteString("  (none yet — press [p] to enter a path or [b] to browse)\n")
		return b.String()
	}
	for i, p := range m.projects {
		cursor := "  "
		name := p.Name
		if i == m.recentIx {
			cursor = cursorStyle.Render("▸ ")
			name = selectedStyle.Render(name)
		}
		b.WriteString(cursor + name + "  " + faintStyle.Render(p.Path) + "\n")
	}
	return b.String()
}

func (m model) viewPath() string {
	var b strings.Builder
	b.WriteString(faintStyle.Render("enter a path (~ expands to home)") + "\n\n")
	box := theme.Box.BorderForeground(theme.Blue)
	b.WriteString(box.Render(m.input+cursorStyle.Render("█")) + "\n")
	return b.String()
}

func (m model) viewBrowse() string {
	var b strings.Builder
	b.WriteString(faintStyle.Render("browsing  "+m.browseDir) + "\n\n")
	if m.browseError != "" {
		b.WriteString(errStyle.Render("  "+m.browseError) + "\n")
		return b.String()
	}
	if len(m.entries) == 0 {
		b.WriteString("  (empty)\n")
		return b.String()
	}
	// Window the list so it fits the terminal height.
	visible := m.height - 10
	if visible < 5 {
		visible = 5
	}
	start := 0
	if m.browseIx >= visible {
		start = m.browseIx - visible + 1
	}
	end := start + visible
	if end > len(m.entries) {
		end = len(m.entries)
	}
	if start > 0 {
		b.WriteString(faintStyle.Render("  ↑ "+strconv.Itoa(start)+" more") + "\n")
	}
	for i := start; i < end; i++ {
		e := m.entries[i]
		cursor := "  "
		label := e.name + "/"
		if e.isUp {
			label = e.name
		}
		if e.isGit {
			label += "  " + gitStyle.Render("(git)")
		}
		if i == m.browseIx {
			cursor = cursorStyle.Render("▸ ")
			label = selectedStyle.Render(label)
		}
		b.WriteString(cursor + label + "\n")
	}
	if end < len(m.entries) {
		b.WriteString(faintStyle.Render("  ↓ "+strconv.Itoa(len(m.entries)-end)+" more") + "\n")
	}
	return b.String()
}

func (m model) footer() string {
	tab := func(label string, active bool) string {
		if active {
			return selectedStyle.Render("[" + label + "]")
		}
		return faintStyle.Render("[" + label + "]")
	}
	left := tab("r]ecent", m.mode == modeRecent) + "  " +
		tab("p]ath", m.mode == modePath) + "  " +
		tab("b]rowse", m.mode == modeBrowse)

	var keys string
	switch m.mode {
	case modePath:
		keys = "enter select  •  esc back"
	case modeBrowse:
		keys = "↑/↓ move  •  enter open dir  •  o select  •  q quit"
	default:
		keys = "↑/↓ move  •  enter select  •  q quit"
	}
	return faintStyle.Render(left + "   •   " + keys)
}
